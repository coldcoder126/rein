package runner

import (
	"context"
	"errors"
	"reflect"
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
	result, err := runSync(r, bg, "c", Text(text))
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
	if summaryReq.Options.MaxTokens != 4096 || summaryReq.Options.CacheRetention != requests[1].Options.CacheRetention {
		t.Fatalf("the summary request should limit output and keep the conversation's options: %+v", summaryReq.Options)
	}
	// Before the first turn, the summary continues the conversation as the previous run left it, without the new input.
	assertContinues(t, summaryReq, requests[1], "回答二")
	if strings.Contains(requestText(summaryReq), "问题三") {
		t.Fatalf("the summary request should not include the new input: %s", requestText(summaryReq))
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

// assertContinues checks that the summary request continues the conversation of prev: the same system prompt, tools and
// options except MaxTokens, prev's messages followed by the given messages (the reply to prev and anything after it, by text),
// and the summary instructions at the end.
func assertContinues(t *testing.T, summaryReq, prev chat.Request, then ...string) {
	t.Helper()
	got, want := summaryReq, prev
	got.MaxTokens, want.MaxTokens = 0, 0
	got.Messages, want.Messages, got.Metadata, want.Metadata = nil, nil, nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the summary request should keep the conversation's system prompt, tools and options:\n%+v\n%+v", got, want)
	}
	n := len(prev.Messages)
	if len(summaryReq.Messages) != n+len(then)+1 || !reflect.DeepEqual(summaryReq.Messages[:n], prev.Messages) {
		t.Fatalf("the summary request should start with the previous request's messages: %s", requestText(summaryReq))
	}
	for i, text := range then {
		if got := summaryReq.Messages[n+i].TextContent(); got != text {
			t.Fatalf("message %d of the summary request should be %q: %q", n+i, text, got)
		}
	}
	if last := summaryReq.Messages[len(summaryReq.Messages)-1]; last.Role != chat.RoleUser || !strings.Contains(last.TextContent(), "Do not call any tools") {
		t.Fatalf("the summary request should end with the summary instructions: %q", last.TextContent())
	}
}

func TestCompactionMidRun(t *testing.T) {
	h, client, _ := compactRunner(t, 1000,
		sayWithUsage("回答一", 100),
		callWithUsage(850), // usage exceeds the threshold after this turn
		chattest.Say("运行中途的摘要"),
		sayWithUsage("最终回答", 200),
	)
	submit(t, h, long("问题一"))
	result := submit(t, h, long("问题二"))
	requests := client.Requests()
	if len(requests) != 4 || result.Final.Text() != "最终回答" {
		t.Fatalf("should compact mid-run and continue: %d %q", len(requests), result.Final.Text())
	}
	// Mid-run, the summary continues the conversation as the next turn would send it: the tool call and its result included.
	assertContinues(t, requests[2], requests[1], "", "工具结果")
	got := requestText(requests[3])
	if !strings.Contains(got, "运行中途的摘要") || strings.Contains(got, "问题一") || !strings.HasSuffix(got, "tool:工具结果") {
		t.Fatalf("the next request after compaction should use the new context and keep the recent tool result: %s", got)
	}
	if result.Usage.TotalTokens != 850+200 {
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
	var starts int
	var turns []int
	result, err := stream(h, bg, "c", Text("问题三"), func(event Event) {
		if ae := event.AgentEvent; ae != nil && ae.Type == agent.EventAgentStart {
			starts++
		} else if ae != nil && ae.Type == agent.EventTurnStart {
			turns = append(turns, ae.TurnIndex)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusCompleted || result.Final.Text() != "恢复了" {
		t.Fatalf("should compact and retry successfully after an overflow: %s %q", result.Status, result.Final.Text())
	}
	if starts != 1 || !slices.Equal(turns, []int{0, 1}) {
		t.Fatalf("recovery should happen within the same Agent run, with consecutive turn numbers: starts=%d turns=%v", starts, turns)
	}
	// On overflow, the summary continues the conversation up to the last successful reply, which is known to fit.
	assertContinues(t, client.Requests()[3], client.Requests()[1], "回答二")
	if got := requestText(client.Requests()[4]); !strings.Contains(got, "超限后的摘要") || !strings.HasSuffix(got, "问题三") {
		t.Fatalf("the retried request should use the compacted context: %s", got)
	}

	_, err = runSync(h, bg, "c", Text("问题四"))
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
	// Manual compaction continues the conversation as the next run would see it.
	summaryReq := client.Requests()[2]
	assertContinues(t, summaryReq, client.Requests()[1], "回答二")
	if last := summaryReq.Messages[len(summaryReq.Messages)-1].TextContent(); !strings.Contains(last, "重点保留数字") {
		t.Fatalf("the summary instructions should carry the extra requirements: %s", last)
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
	// Within the conversation only MaxTokens applies; a separate summary model gets a transcript with every configured parameter.
	options := chat.Options{MaxTokens: 100, CacheRetention: chat.CacheRetentionShort, Reasoning: chat.ReasoningLow}
	summarizer := newClient(chattest.Say("摘要"))
	for _, c := range []SummaryCompactor{{Options: options}, {Options: options, Client: summarizer}} {
		custom := newClient(chattest.Say("回答"), chattest.Say("回答"), chattest.Say("摘要"))
		r, _ := newRunner(t, custom, func(cfg *Config) { cfg.Compactor = c })
		for range 2 {
			if _, err := runSync(r, bg, "c", Text("问题")); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := r.Compact(bg, "c", CompactOptions{}); err != nil {
			t.Fatal(err)
		}
		if c.Client == nil {
			if got := custom.Requests()[2].Options; got.MaxTokens != 100 || got.CacheRetention != "" || got.Reasoning != "" {
				t.Fatalf("within the conversation the summary request should only change MaxTokens: %+v", got)
			}
			continue
		}
		got := summarizer.Requests()[0]
		if len(custom.Requests()) != 2 || got.MaxTokens != 100 || got.CacheRetention != chat.CacheRetentionShort || got.Reasoning != chat.ReasoningLow ||
			got.SystemPrompt != defaultSummaryPrompt || !strings.Contains(got.Messages[0].TextContent(), "<conversation>") {
			t.Fatalf("a separate summary model should get a transcript with the configured parameters: %+v", got)
		}
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
		if _, err := runSync(r, bg, "c", Text(long(q)), WithModel(other)); err != nil {
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

func TestManualCompactionIsARun(t *testing.T) {
	h, _, _ := compactRunner(t, 0, chattest.Say("回答一"), chattest.Say("回答二"), chattest.Say("摘要"))
	submit(t, h, "问题一")
	submit(t, h, "问题二")
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	followed := make(chan Event, 16)
	go func() {
		for event := range h.Subscribe(ctx, "c") {
			followed <- event
		}
	}()
	waitFor(t, "订阅就绪", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.followers["c"] != nil
	})
	summary, _, err := h.Compact(bg, "c", CompactOptions{})
	if err != nil || summary == nil {
		t.Fatal(err)
	}
	var types []string
	var end Event
	for end.Type != EventRunEnd {
		end = <-followed
		types = append(types, string(end.Type))
	}
	if got := strings.Join(types, ","); got != "run_start,message_saved,compaction,run_end" {
		t.Fatalf("manual compaction should have the events of a run: %s", got)
	}
	if res := end.Result; res.Status != agent.StatusCompleted || len(res.Messages) != 1 || res.Messages[0].ID != summary.ID || res.RunID != summary.RunID {
		t.Fatalf("manual compaction should end with a Result holding the summary: %+v", res)
	}
}

func TestCompactionContinuesWithRunContextSteeringAndRewrites(t *testing.T) {
	var sideCalls []bool
	var h *Runner
	var steer bool
	client := newClient(
		sayWithUsage("回答一", 100),
		callWithUsage(300), callWithUsage(850), // the second call brings usage over the threshold
		chattest.Say("中途摘要"),
		sayWithUsage("最终回答", 100),
	)
	client.Model.MaxInputTokens = 1000
	h, _ = newRunner(t, client, func(cfg *Config) {
		cfg.Middlewares = []agent.Middleware{
			// A runtime middleware that rewrites every request: the summary must see the same rewrite.
			agent.BeforeModel(func(_ context.Context, turn agent.TurnInfo, req *chat.Request) error {
				sideCalls = append(sideCalls, turn.SideCall)
				req.SystemPrompt += "（动态）"
				req.Tools = append(req.Tools, chat.ToolDef{Name: "dynamic", InputSchema: []byte(`{"type":"object"}`)})
				return nil
			}),
			{WrapTurn: func(ctx context.Context, turn agent.TurnInfo, next agent.TurnNext) (*agent.TurnResult, error) {
				result, err := next(ctx)
				if steer && turn.TurnIndex == 0 {
					_ = h.Current("c").Steer(Text("补充说明"))
				}
				return result, err
			}},
		}
	})
	submit(t, h, long("问题一"))
	steer = true
	result, err := runSync(h, bg, "c", Text(long("问题二")), WithContext(ContextInput{Text: "页面：设置"}))
	if err != nil || result.Final.Text() != "最终回答" {
		t.Fatalf("should compact mid-run and finish: %v %+v", err, result)
	}
	requests := client.Requests()
	if got := requestText(requests[2]); !strings.Contains(got, "页面：设置") || !strings.Contains(got, "补充说明") {
		t.Fatalf("the turn before compaction should carry the run context and the steering message: %s", got)
	}
	// The conversation rebuilt from the store matches the request the turn sent, run context and steering in place.
	assertContinues(t, requests[3], requests[2], "", "工具结果")
	if !slices.Equal(sideCalls, []bool{false, false, false, true, false}) {
		t.Fatalf("only the summary should be a side call: %v", sideCalls)
	}
	if got := requestText(requests[4]); !strings.Contains(got, "中途摘要") || !strings.Contains(got, "页面：设置") {
		t.Fatalf("the run context should stay after compaction: %s", got)
	}
}

func TestSummaryCompactorFallsBackToTranscript(t *testing.T) {
	messages := []session.Message{
		{ID: "u1", Type: session.TypeUser, ChatMessage: chat.NewUserTextMessage(long("问题一"))},
		{ID: "a1", Type: session.TypeAssistant, ChatMessage: chat.Message{Role: chat.RoleAssistant, Content: []chat.ContentBlock{chat.Text("回答一")}}},
		{ID: "u2", Type: session.TypeUser, ChatMessage: chat.NewUserTextMessage(long("问题二"))},
	}
	client := newClient(chattest.Say("纯文本摘要"))
	in := CompactInput{Messages: messages, Reason: CompactManual, Client: client}
	result, err := SummaryCompactor{}.Compact(bg, in)
	if err != nil || result.Summary != "纯文本摘要" || !strings.Contains(client.Requests()[0].Messages[0].TextContent(), "<conversation>") {
		t.Fatalf("without Continue the summary should come from a transcript: %+v %v", result, err)
	}

	in.ContinueThrough = "a1"
	in.Continue = func(context.Context, []chat.Message, agent.SideCallOptions) (*chat.Message, error) {
		return &chat.Message{Role: chat.RoleAssistant, Content: []chat.ContentBlock{{Type: chat.ContentTypeToolCall, ToolCall: &chat.ToolCall{ID: "1", Name: "lookup"}}}}, nil
	}
	if _, err := (SummaryCompactor{}).Compact(bg, in); err == nil || !strings.Contains(err.Error(), "no summary text") {
		t.Fatalf("a reply with only tool calls should fail: %v", err)
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
	if _, err := runSync(h, bg, "c", Text("问")); err == nil || !strings.Contains(err.Error(), "FirstKeptID") {
		t.Fatalf("an invalid kept start should fail: %v", err)
	}
	failing := &fixedCompactor{result: func(CompactInput) (*CompactResult, error) { return nil, errors.New("摘要服务不可用") }}
	h, _ = newRunner(t, client, with(failing))
	if _, err := runSync(h, bg, "c", Text("问")); err == nil {
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
