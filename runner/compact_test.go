package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
	"github.com/coldcoder126/rein/session"
)

// sayWithUsage produces some text and reports the given context usage.
func sayWithUsage(text string, tokens int) chattest.Script {
	return func(_ context.Context, b *chat.Builder) {
		b.SetUsage(chat.Usage{Input: tokens, TotalTokens: tokens})
		b.TextDelta(text)
		b.Done("")
	}
}

func callWithUsage(tokens int) chattest.Script {
	return func(_ context.Context, b *chat.Builder) {
		b.SetUsage(chat.Usage{Input: tokens, TotalTokens: tokens})
		b.ToolCall("c1", "lookup", nil)
		b.Done("")
	}
}

// compactRunner creates a Runner whose context limit is limit; the session ID is always "c".
func compactRunner(t *testing.T, limit int, scripts ...chattest.Script) (*Runner, *scriptedClient, *session.MemoryStore) {
	t.Helper()
	client := newClient(scripts...)
	client.Model.MaxInputTokens = limit
	r, store := newRunner(t, client)
	return r, client, store
}

func submit(t *testing.T, r *Runner, text string) *Result {
	t.Helper()
	result, err := r.Run(bg, "c", Text(text))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// long returns a tagged long text (about 300 tokens), so the size estimated from content matches the usage set by the test.
func long(tag string) string { return tag + strings.Repeat("。", 300) }

func requestText(req chat.Request) string {
	var parts []string
	for _, msg := range req.Messages {
		parts = append(parts, string(msg.Role)+":"+msg.TextContent())
	}
	return strings.Join(parts, " | ")
}

func TestCompactionBeforeRun(t *testing.T) {
	h, client, store := compactRunner(t, 1000,
		sayWithUsage("回答一", 300),
		sayWithUsage("回答二", 900), // usage reaches 90% after the second run
		chattest.Say("这是摘要"),     // the summary request before the third submit
		sayWithUsage("回答三", 200),
		sayWithUsage("回答四", 250),
	)
	var compactions int
	submit(t, h, long("问题一"))
	submit(t, h, long("问题二"))
	if len(client.Requests()) != 2 {
		t.Fatalf("should not compact when usage is under the threshold: %d", len(client.Requests()))
	}
	if _, err := stream(h, bg, "c", Text(long("问题三")), func(event Event) {
		if event.Type == EventCompaction {
			compactions++
		}
	}); err != nil {
		t.Fatal(err)
	}

	requests := client.Requests()
	summaryReq := requests[2]
	if summaryReq.Options.MaxTokens != 4096 || summaryReq.Options.CacheRetention != chat.CacheRetentionNone {
		t.Fatalf("the summary request should limit output and not cache by default: %+v", summaryReq.Options)
	}
	if len(summaryReq.Tools) != 0 || !strings.Contains(summaryReq.Messages[0].TextContent(), "问题一") || strings.Contains(summaryReq.Messages[0].TextContent(), "问题三") {
		t.Fatalf("the summary request should only contain the old messages being summarized: %s", summaryReq.Messages[0].TextContent())
	}
	got := requestText(requests[3])
	if !strings.Contains(got, "<conversation_summary>\n这是摘要\n</conversation_summary>") || strings.Contains(got, "问题一") || !strings.HasSuffix(got, long("问题三")) {
		t.Fatalf("the request after compaction should be the summary plus the recent messages: %s", got)
	}
	all, _ := store.ListMessages(bg, "c")
	var summaries int
	for _, msg := range all {
		if msg.Type == session.TypeSummary {
			summaries++
			if msg.Visible || msg.FirstKeptID == "" {
				t.Fatalf("the summary message should be hidden and record where the kept part starts: %+v", msg)
			}
		}
	}
	if summaries != 1 || compactions != 1 || len(session.DisplayMessages(all)) != 6 {
		t.Fatalf("all original messages should be kept with one extra summary: summaries=%d events=%d display=%d", summaries, compactions, len(session.DisplayMessages(all)))
	}

	// Usage from before the summary (900) must not be used for the estimate, or the next run would compact again right away.
	submit(t, h, long("问题四"))
	if n := len(client.Requests()); n != 5 {
		t.Fatalf("should not compact again right after compacting: %d requests", n)
	}
}

func TestCompactionMidRun(t *testing.T) {
	h, client, _ := compactRunner(t, 1000,
		sayWithUsage("回答一", 100),
		callWithUsage(950), // usage exceeds the threshold after this turn
		chattest.Say("运行中途的摘要"),
		sayWithUsage("最终回答", 200),
	)
	submit(t, h, long("问题一"))
	result := submit(t, h, long("问题二"))
	requests := client.Requests()
	if len(requests) != 4 || result.Final.Text() != "最终回答" {
		t.Fatalf("should compact mid-run and continue: %d %q", len(requests), result.Final.Text())
	}
	got := requestText(requests[3])
	if !strings.Contains(got, "运行中途的摘要") || strings.Contains(got, "问题一") || !strings.HasSuffix(got, "tool:工具结果") {
		t.Fatalf("the next request after compaction should use the new context and keep the recent tool result: %s", got)
	}
	if result.Usage.TotalTokens != 950+200 {
		t.Fatalf("usage should accumulate: %+v", result.Usage)
	}
}

func TestCompactionOnOverflowRetriesOnce(t *testing.T) {
	overflow := chattest.Fail(&chat.Error{Kind: chat.KindContextLength, Message: "prompt is too long"})
	h, client, _ := compactRunner(t, 1_000_000,
		sayWithUsage("回答一", 100), sayWithUsage("回答二", 100),
		overflow, chattest.Say("超限后的摘要"), sayWithUsage("恢复了", 100),
		overflow, chattest.Say("第二次摘要"), overflow,
	)
	submit(t, h, "问题一")
	submit(t, h, "问题二")
	var result *Result
	var starts int
	var turns []int
	for event, err := range h.Stream(bg, "c", Text("问题三")) {
		if err != nil {
			t.Fatal(err)
		}
		if ae := event.AgentEvent; ae != nil && ae.Type == agent.EventAgentStart {
			starts++
		} else if ae != nil && ae.Type == agent.EventTurnStart {
			turns = append(turns, ae.TurnIndex)
		}
		if event.Result != nil {
			result = event.Result
		}
	}
	if result.Status != agent.StatusCompleted || result.Final.Text() != "恢复了" {
		t.Fatalf("should compact and retry successfully after an overflow: %s %q", result.Status, result.Final.Text())
	}
	if starts != 1 || !slices.Equal(turns, []int{0, 1}) {
		t.Fatalf("recovery should happen within the same Agent run, with consecutive turn numbers: starts=%d turns=%v", starts, turns)
	}
	if got := requestText(client.Requests()[4]); !strings.Contains(got, "超限后的摘要") || !strings.HasSuffix(got, "问题三") {
		t.Fatalf("the retried request should use the compacted context: %s", got)
	}

	_, err := h.Run(bg, "c", Text("问题四"))
	if ce := chat.AsError(err); ce == nil || ce.Kind != chat.KindContextLength || len(client.Requests()) != 8 {
		t.Fatalf("an overflow after the retry should return an error, retrying only once: %v requests %d", err, len(client.Requests()))
	}
}

func TestManualCompaction(t *testing.T) {
	// The model declares no context limit: no automatic compaction, but manual compaction still works.
	h, client, store := compactRunner(t, 0,
		chattest.Say("回答一"), chattest.Say("回答二"),
		sayWithUsage("手动摘要", 42),
		chattest.Say("回答三"),
	)
	submit(t, h, "问题一")
	submit(t, h, "问题二")
	summary, usage, err := h.Compact(bg, "c", CompactOptions{Instructions: "重点保留数字"})
	if err != nil || usage.TotalTokens != 42 || summary == nil || summary.Type != session.TypeSummary || summary.Text() != "手动摘要" || summary.FirstKeptID == "" {
		t.Fatalf("should generate and return the stored summary and its usage: %+v %+v %v", summary, usage, err)
	}
	summaryReq := client.Requests()[2]
	if !strings.Contains(summaryReq.SystemPrompt, "重点保留数字") || !strings.Contains(summaryReq.Messages[0].TextContent(), "问题一") || strings.Contains(summaryReq.Messages[0].TextContent(), "问题二") {
		t.Fatalf("the summary request should carry the extra requirements and keep the recent messages: %s | %s", summaryReq.SystemPrompt, summaryReq.Messages[0].TextContent())
	}
	all, _ := store.ListMessages(bg, "c")
	if last := all[len(all)-1]; last.ID != summary.ID || summary.RunID == "" || summary.RunID == all[0].RunID {
		t.Fatalf("the summary should be stored with the manual compaction's own RunID: %+v", last)
	}
	submit(t, h, "问题三")
	if got := requestText(client.Requests()[3]); !strings.Contains(got, "手动摘要") || strings.Contains(got, "问题一") || !strings.Contains(got, "问题二") {
		t.Fatalf("later runs should use the compacted context: %s", got)
	}

	busy, _ := h.acquire("c", func() {}, false)
	if _, _, err := h.Compact(bg, "c", CompactOptions{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("should return ErrBusy while the session is running: %v", err)
	}
	h.release("c", busy)
	// Custom summary request parameters are used as is.
	custom := newClient(chattest.Say("回答"), chattest.Say("回答"), chattest.Say("摘要"))
	options := chat.Options{MaxTokens: 100, CacheRetention: chat.CacheRetentionShort, Reasoning: chat.ReasoningLow}
	r, _ := newRunner(t, custom, func(cfg *Config) { cfg.Compactor = SummaryCompactor{Options: options} })
	for range 2 {
		if _, err := r.Run(bg, "c", Text("问题")); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := r.Compact(bg, "c", CompactOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := custom.Requests()[2].Options; got.MaxTokens != 100 || got.CacheRetention != chat.CacheRetentionShort || got.Reasoning != chat.ReasoningLow {
		t.Fatalf("the summary request should use the configured parameters: %+v", got)
	}

	off, _ := newRunner(t, newClient(), func(cfg *Config) { cfg.DisableCompaction = true })
	if _, _, err := off.Compact(bg, "c", CompactOptions{}); err == nil {
		t.Fatal("should return an error when compaction is disabled")
	}
}

func TestCompactionFollowsTheRunModel(t *testing.T) {
	// The default model declares no context limit, so only the run's model can trigger compaction.
	base := newClient()
	other := newClient(sayWithUsage("回答一", 300), sayWithUsage("回答二", 900), chattest.Say("这是摘要"), chattest.Say("回答三"), chattest.Say("手动摘要"))
	other.Model.MaxInputTokens = 1000
	r, _ := newRunner(t, base)
	for _, q := range []string{"问题一", "问题二", "问题三"} {
		if _, err := r.Run(bg, "c", Text(long(q)), WithModel(other)); err != nil {
			t.Fatal(err)
		}
	}
	if requests := other.Requests(); len(requests) != 4 || !strings.Contains(requestText(requests[3]), "这是摘要") {
		t.Fatalf("compaction should use the run's model for its limit and the summary: %d requests", len(requests))
	}
	if summary, _, err := r.Compact(bg, "c", CompactOptions{Model: other}); err != nil || summary == nil || summary.Text() != "手动摘要" {
		t.Fatalf("manual compaction should use the given model: %+v %v", summary, err)
	}
	if n := len(base.Requests()); n != 0 {
		t.Fatalf("the default model should not be called: %d requests", n)
	}
}

type fixedCompactor struct {
	calls  int
	result func(in CompactInput) (*CompactResult, error)
}

func (c *fixedCompactor) Compact(_ context.Context, in CompactInput) (*CompactResult, error) {
	c.calls++
	return c.result(in)
}

func TestCustomCompactorAndDisable(t *testing.T) {
	custom := &fixedCompactor{result: func(in CompactInput) (*CompactResult, error) {
		if len(in.Messages) < 3 {
			return nil, nil
		}
		return &CompactResult{Summary: "自定义摘要", FirstKeptID: in.Messages[len(in.Messages)-1].ID}, nil
	}}
	client := newClient(chattest.Say("一"), chattest.Say("二"))
	client.Model.MaxInputTokens = 1000
	with := func(c Compactor) func(*Config) { return func(cfg *Config) { cfg.Compactor = c } }
	h, _ := newRunner(t, client, with(custom))
	submit(t, h, "问题一")
	submit(t, h, "问题二")
	if got := requestText(client.Requests()[1]); got != "user:<conversation_summary>\n自定义摘要\n</conversation_summary>\n\n问题二" || custom.calls != 2 {
		t.Fatalf("should use the result of the custom strategy: %q calls=%d", got, custom.calls)
	}

	bad := &fixedCompactor{result: func(CompactInput) (*CompactResult, error) {
		return &CompactResult{Summary: "x", FirstKeptID: "不存在"}, nil
	}}
	h, _ = newRunner(t, client, with(bad))
	if _, err := h.Run(bg, "c", Text("问")); err == nil || !strings.Contains(err.Error(), "FirstKeptID") {
		t.Fatalf("an invalid kept start should fail: %v", err)
	}
	failing := &fixedCompactor{result: func(CompactInput) (*CompactResult, error) { return nil, errors.New("摘要服务不可用") }}
	h, _ = newRunner(t, client, with(failing))
	if _, err := h.Run(bg, "c", Text("问")); err == nil {
		t.Fatal("a strategy error should stop the run")
	}

	off := newClient(sayWithUsage("一", 990), chattest.Say("二"))
	off.Model.MaxInputTokens = 1000
	h, _ = newRunner(t, off, func(cfg *Config) { cfg.DisableCompaction = true })
	submit(t, h, "问题一")
	submit(t, h, "问题二")
	if len(off.Requests()) != 2 {
		t.Fatalf("no summary requests after disabling compaction: %d", len(off.Requests()))
	}
}

func TestCutIndexAndEffectiveMessages(t *testing.T) {
	big := strings.Repeat("字", 100) // about 100 tokens
	msg := func(id string, role session.MessageType) session.Message {
		return session.Message{ID: id, Type: role, ChatMessage: chat.Message{Content: []chat.ContentBlock{chat.Text(big)}}}
	}
	messages := []session.Message{
		msg("u1", session.TypeUser), msg("a1", session.TypeAssistant), msg("t1", session.TypeTool),
		msg("a2", session.TypeAssistant), msg("t2", session.TypeTool), msg("a3", session.TypeAssistant),
	}
	// One question leads to a long tool chain: with no user message within budget going back, align forward to an assistant message, never starting at a tool result.
	if cut := cutIndex(messages, 150, false); messages[cut].ID != "a3" {
		t.Fatalf("the cut should align forward to an assistant message: %s", messages[cut].ID)
	}
	if cutIndex(messages, 100_000, false) != 0 {
		t.Fatal("should not compact when everything fits within the keep budget")
	}
	if cut := cutIndex(messages, 100_000, true); messages[cut].ID != "a2" {
		t.Fatalf("forced compaction should cut in half and align: %s", messages[cut].ID)
	}
	// Several exchanges: the cut aligns back to a user message, so the kept part starts with a complete exchange.
	chatLog := []session.Message{
		msg("u1", session.TypeUser), msg("a1", session.TypeAssistant),
		msg("u2", session.TypeUser), msg("a2", session.TypeAssistant),
		msg("u3", session.TypeUser), msg("a3", session.TypeAssistant),
	}
	if cut := cutIndex(chatLog, 150, false); chatLog[cut].ID != "u3" {
		t.Fatalf("the cut should align to a user message: %s", chatLog[cut].ID)
	}

	summary := session.Message{ID: "s1", Type: session.TypeSummary, FirstKeptID: "a2"}
	all := append(append([]session.Message{}, messages...), summary, msg("u2", session.TypeUser))
	out, fresh := effectiveMessages(all)
	var ids []string
	for _, m := range out {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "s1,a2,t2,a3,u2" || fresh != 4 {
		t.Fatalf("should be the summary, the kept messages and the new messages: %v fresh=%d", ids, fresh)
	}
}
