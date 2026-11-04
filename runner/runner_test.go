package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
	"github.com/coldcoder126/rein/session"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/shell"
	tasktool "github.com/coldcoder126/rein/tool/task"
)

var bg = context.Background()

type scriptedClient struct {
	*chattest.Client
	started chan struct{}
	release chan struct{}
}

func newClient(scripts ...chattest.Script) *scriptedClient {
	return &scriptedClient{Client: chattest.NewClient(scripts...)}
}

// newGatedClient makes the first model call notify started, then wait for release before producing output.
func newGatedClient(scripts ...chattest.Script) *scriptedClient {
	c := &scriptedClient{started: make(chan struct{}, 1), release: make(chan struct{})}
	if len(scripts) > 0 {
		scripts[0] = chattest.Gate(c.started, c.release, scripts[0])
	}
	c.Client = chattest.NewClient(scripts...)
	return c
}

func lookupTool() tool.Tool {
	return tool.New("lookup", "查询固定数据", func(*tool.Context, struct{}) (*tool.Result, error) {
		return tool.Text("工具结果"), nil
	})
}

// newRunner creates a Runner with an in-memory store and the lookup tool; mods can adjust the Agent or the config.
func newRunner(t *testing.T, model chat.Client, mods ...func(*Config)) (*Runner, *session.MemoryStore) {
	t.Helper()
	store := session.NewMemoryStore()
	cfg := Config{Store: store, Agent: &agent.Agent{Model: model, Tools: []tool.Tool{lookupTool()}}}
	for _, mod := range mods {
		mod(&cfg)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r, store
}

func saved(t *testing.T, store session.Store, sessionID string) []session.Message {
	t.Helper()
	messages, err := store.ListMessages(bg, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

func sessionRoles(msgs []session.Message) string {
	var out []string
	for _, msg := range msgs {
		out = append(out, string(msg.Type))
	}
	return strings.Join(out, ",")
}

// lookupThenAnswer is a two-step script: call the tool once, then answer.
func lookupThenAnswer(answer string) []chattest.Script {
	return []chattest.Script{chattest.CallTools(chat.ToolCall{ID: "c1", Name: "lookup"}), chattest.Say(answer)}
}

type outcome struct {
	result *Result
	err    error
}

// runSync starts a run and waits for its result.
func runSync(r *Runner, ctx context.Context, sessionID string, in Input, opts ...RunOption) (*Result, error) {
	run, err := r.Start(ctx, sessionID, in, opts...)
	if err != nil {
		return nil, err
	}
	return run.Wait(ctx)
}

// stream starts a run, passes each of its events to on, and returns its result.
func stream(r *Runner, ctx context.Context, sessionID string, in Input, on func(Event), opts ...RunOption) (*Result, error) {
	run, err := r.Start(ctx, sessionID, in, opts...)
	if err != nil {
		return nil, err
	}
	for event := range run.Events(ctx) {
		if on != nil {
			on(event)
		}
	}
	return run.Wait(ctx)
}

// runAsync starts a run in the background.
func runAsync(r *Runner, sessionID, text string) <-chan outcome {
	done := make(chan outcome, 1)
	go func() {
		result, err := runSync(r, bg, sessionID, Text(text))
		done <- outcome{result, err}
	}()
	return done
}

func TestRunPersistsMessagesAndContext(t *testing.T) {
	client := newClient(lookupThenAnswer("AAPL 估值合理")...)
	r, store := newRunner(t, client, func(c *Config) { c.Agent.Instruction = "你是助手" })
	result, err := runSync(r, bg, "s1", Text("这家公司便宜吗？"),
		WithContext(
			ContextInput{Text: "当前股票：AAPL"},
			ContextInput{Text: "用户确认：长期关注 AAPL", Kind: session.KindNormal, Scope: session.ScopePersistent},
		),
		WithDetails(map[string]string{"page": "valuation"}),
	)
	if err != nil || result.Status != agent.StatusCompleted || result.SessionID != "s1" || result.RunID == "" {
		t.Fatalf("should complete normally: %+v %v", result, err)
	}
	all := saved(t, store, "s1")
	if got := sessionRoles(all); got != "context,user,assistant,tool,assistant" {
		t.Fatalf("should store the persistent context, user message, assistant and tool messages, but not run-scoped context: %s", got)
	}
	if all[1].Details == nil || all[3].Visible || all[3].ChatMessage.ToolCallID != "c1" {
		t.Fatalf("the user message should carry Details, and tool results be hidden with the raw message kept: %+v", all)
	}
	for _, msg := range all {
		if msg.RunID != result.RunID {
			t.Fatalf("every message written by the run should carry its RunID: %+v", msg)
		}
	}
	display := session.DisplayMessages(all)
	if len(display) != 2 || display[0].Text() != "这家公司便宜吗？" || result.Final.Text() != "AAPL 估值合理" {
		t.Fatalf("display messages should only be the user question and the final answer: %+v", display)
	}
	req := client.Requests()[0]
	first := req.Messages[0].TextContent()
	if req.SystemPrompt != "你是助手" || len(req.Messages) != 1 || !strings.Contains(first, "当前股票：AAPL") ||
		!strings.Contains(first, "长期关注") || !strings.HasSuffix(first, "这家公司便宜吗？") {
		t.Fatalf("both kinds of context should be merged into the user message sent to the model: %q", first)
	}

	// Second run: the run-scoped context is gone, the persistent one remains.
	if _, err := runSync(r, bg, "s1", Text("继续")); err != nil {
		t.Fatal(err)
	}
	second := client.Requests()[2].Messages
	if text := second[0].TextContent(); strings.Contains(text, "当前股票") || !strings.Contains(text, "长期关注") {
		t.Fatalf("run-scoped context should only enter its own run: %q", text)
	}
}

func TestSessionsAreIsolatedAndRunConcurrently(t *testing.T) {
	client := newGatedClient(chattest.Say("回答甲"), chattest.Say("回答乙"))
	r, store := newRunner(t, client)
	first := runAsync(r, "甲", "甲的问题")
	<-client.started

	if _, err := runSync(r, bg, "甲", Text("同一会话的第二个请求")); !errors.Is(err, ErrBusy) {
		t.Fatalf("a session should have only one run at a time: %v", err)
	}
	// Another session is unaffected and can finish while the first is still running.
	if result, err := runSync(r, bg, "乙", Text("乙的问题")); err != nil || result.Final.Text() != "回答乙" {
		t.Fatalf("different sessions should run concurrently: %+v %v", result, err)
	}
	if !r.Running("甲") || r.Running("乙") {
		t.Fatal("the state should reflect whether each session is running")
	}
	close(client.release)
	if out := <-first; out.err != nil || out.result.Final.Text() != "回答甲" {
		t.Fatalf("the first session should complete normally: %+v %v", out.result, out.err)
	}
	if a, b := saved(t, store, "甲"), saved(t, store, "乙"); len(a) != 2 || len(b) != 2 || a[0].Text() != "甲的问题" || b[0].Text() != "乙的问题" {
		t.Fatalf("session histories should be independent: %+v %+v", a, b)
	}

	auto, err := runSync(r, bg, "", Text("没有指定会话"))
	if err != nil || !strings.HasPrefix(auto.SessionID, "session_") || len(saved(t, store, auto.SessionID)) != 2 {
		t.Fatalf("an empty session ID should be generated: %+v %v", auto, err)
	}
}

func TestSteerIsInjectedBetweenTurnsAndFollowUpAtTheEnd(t *testing.T) {
	scripts := append(lookupThenAnswer("已按新要求处理"), chattest.Say("后续任务完成"))
	client := newGatedClient(scripts...)
	r, store := newRunner(t, client)
	done := runAsync(r, "s", "开始")
	<-client.started
	// Queue while the first model call has not returned: follow-up first, then steer, but steer should be handled first.
	if err := r.FollowUp("s", Text("再做一件事")); err != nil {
		t.Fatal(err)
	}
	if err := r.FollowUp("s", Text("还有一件")); err != nil {
		t.Fatal(err)
	}
	if err := r.Steer("s", Text("改一下方向")); err != nil {
		t.Fatal(err)
	}
	close(client.release)
	out := <-done
	if out.err != nil || out.result.Status != agent.StatusCompleted {
		t.Fatalf("should complete normally: %+v %v", out.result, out.err)
	}
	requests := client.Requests()
	if len(requests) != 3 {
		t.Fatalf("one run should have three model calls: %d", len(requests))
	}
	second := requests[1].Messages
	if second[len(second)-1].TextContent() != "改一下方向" || second[len(second)-2].Role != chat.RoleTool {
		t.Fatalf("steer should be injected after the tool results and before the next model call: %+v", second)
	}
	if third := requests[2].Messages; third[len(third)-1].TextContent() != "再做一件事\n\n还有一件" {
		t.Fatalf("consecutive follow-ups should merge into one model message: %+v", third)
	}
	if got := sessionRoles(saved(t, store, "s")); got != "user,assistant,tool,user,assistant,user,user,assistant" {
		t.Fatalf("messages should be stored in the order they entered the context, each follow-up kept separately: %s", got)
	}
	if len(out.result.Pending.Steer)+len(out.result.Pending.FollowUp) != 0 {
		t.Fatalf("nothing should be pending once everything is processed: %+v", out.result.Pending)
	}
	if err := r.Steer("s", Text("太晚了")); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("queuing while idle should return ErrNotRunning: %v", err)
	}
}

func TestAbortReturnsQueuedMessagesWithoutSavingThem(t *testing.T) {
	client := newGatedClient(chattest.Say("不会到达"))
	r, store := newRunner(t, client)
	var ended Event
	var queues []Queue
	done := make(chan outcome, 1)
	go func() {
		result, err := stream(r, bg, "s", Text("开始"), func(event Event) {
			switch event.Type {
			case EventRunEnd:
				ended = event
			case EventQueueUpdate:
				queues = append(queues, *event.Queue)
			}
		})
		done <- outcome{result, err}
	}()
	<-client.started
	_ = r.Steer("s", Text("转向"))
	_ = r.FollowUp("s", Text("后续"))
	pending := r.Abort("s")
	out := <-done

	if len(pending.Steer) != 1 || pending.Steer[0].Text() != "转向" || len(pending.FollowUp) != 1 || pending.FollowUp[0].Text() != "后续" {
		t.Fatalf("Abort should return the messages not yet injected: %+v", pending)
	}
	if !errors.Is(out.err, context.Canceled) || out.result.Status != agent.StatusAborted || ended.Result != out.result {
		t.Fatalf("the run should end as aborted, reported in run_end: %v %s %+v", out.err, out.result.Status, ended)
	}
	if len(queues) == 0 || len(queues[len(queues)-1].Steer)+len(queues[len(queues)-1].FollowUp) != 0 {
		t.Fatalf("after queues change outside the run, the run should emit queue_update: %+v", queues)
	}
	all := saved(t, store, "s")
	if got := sessionRoles(all); got != "user,assistant" || all[1].ChatMessage.StopReason != chat.StopReasonAborted {
		t.Fatalf("the canceled assistant message should be stored, queued messages should not enter history: %s", got)
	}
	if q := r.Abort("s"); q.Steer != nil || q.FollowUp != nil {
		t.Fatal("Abort while idle should return an empty queue")
	}
	if err := r.Wait(bg, "s"); err != nil {
		t.Fatalf("Wait while idle should return immediately: %v", err)
	}
}

func TestMaxTurnsLeavesUnprocessedMessagesPending(t *testing.T) {
	client := newGatedClient(lookupThenAnswer("不应到达")...)
	r, store := newRunner(t, client, func(c *Config) { c.Agent.MaxTurns = 1 })
	done := runAsync(r, "s", "开始")
	<-client.started
	_ = r.Steer("s", Text("来不及处理"))
	close(client.release)
	out := <-done
	if !errors.Is(out.err, agent.ErrMaxTurnsExceeded) || out.result.Status != agent.StatusMaxTurns {
		t.Fatalf("should end with max_turns: %s %v", out.result.Status, out.err)
	}
	if len(out.result.Pending.Steer) != 1 || out.result.Pending.Steer[0].Text() != "来不及处理" {
		t.Fatalf("messages taken but not injected should be returned as pending: %+v", out.result.Pending)
	}
	if got := sessionRoles(saved(t, store, "s")); got != "user,assistant,tool" {
		t.Fatalf("messages not injected should not be stored: %s", got)
	}
}

func TestModelFailureIsReportedAndExcludedFromReplay(t *testing.T) {
	client := newClient(
		func(_ context.Context, b *chat.Builder) {
			b.TextDelta("半截")
			b.Fail(&chat.Error{Kind: chat.KindServer, Message: "上游过载"})
		},
		chattest.Say("恢复了"),
	)
	r, store := newRunner(t, client)
	result, err := runSync(r, bg, "s", Text("第一问"))
	var ce *chat.Error
	if !errors.As(err, &ce) || ce.Kind != chat.KindServer || result.Status != agent.StatusFailed {
		t.Fatalf("a model failure should return a *chat.Error: %v %s", err, result.Status)
	}
	last := saved(t, store, "s")[1]
	if last.ChatMessage.StopReason != chat.StopReasonError || last.Text() != "半截" {
		t.Fatalf("the failed assistant message should be stored with the content generated so far: %+v", last)
	}
	if _, err := runSync(r, bg, "s", Text("第二问")); err != nil {
		t.Fatal(err)
	}
	// The Runner passes error-marked history messages through unchanged; providers remove them with NormalizeForModel.
	for _, msg := range chat.NormalizeForModel(client.Requests()[1].Messages, chat.ModelInfo{Provider: "stub", Model: "stub"}) {
		if msg.Role == chat.RoleAssistant {
			t.Fatalf("failed assistant messages should not survive normalization: %+v", msg)
		}
	}
}

func TestStoreFailureEndsRunAsFailed(t *testing.T) {
	r, _ := newRunner(t, newClient(chattest.Say("不应到达")), func(c *Config) { c.Store = failingStore{session.NewMemoryStore()} })
	result, err := runSync(r, bg, "s", Text("开始"))
	if err == nil || result.Status != agent.StatusFailed {
		t.Fatalf("a store failure should return an error and be marked failed: %+v %v", result, err)
	}
	if _, err := runSync(r, bg, "s", Text("再来")); errors.Is(err, ErrBusy) {
		t.Fatal("the session should be idle again after the failure")
	}
}

func TestAbortedMessageIsSavedByStoreThatHonorsContext(t *testing.T) {
	client := newGatedClient(chattest.Say("不会到达"))
	store := ctxStore{session.NewMemoryStore()}
	r, _ := newRunner(t, client, func(c *Config) { c.Store = store })
	done := make(chan outcome, 1)
	go func() {
		result, err := runSync(r, bg, "s", Text("开始"))
		done <- outcome{result, err}
	}()
	<-client.started
	r.Abort("s")
	out := <-done

	if !errors.Is(out.err, context.Canceled) || out.result.Status != agent.StatusAborted {
		t.Fatalf("the run should end as aborted: %v %s", out.err, out.result.Status)
	}
	all, err := store.ListMessages(bg, "s")
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionRoles(all); got != "user,assistant" || all[1].ChatMessage.StopReason != chat.StopReasonAborted {
		t.Fatalf("the assistant message should still be stored after canceling: %s", got)
	}
}

// ctxStore rejects writes when ctx is canceled, like a database driver.
type ctxStore struct{ *session.MemoryStore }

func (s ctxStore) AppendMessages(ctx context.Context, sessionID string, messages ...session.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.MemoryStore.AppendMessages(ctx, sessionID, messages...)
}

type failingStore struct{ *session.MemoryStore }

func (failingStore) AppendMessages(context.Context, string, ...session.Message) error {
	return errors.New("磁盘已满")
}

func TestRunWithImage(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	client := newClient(chattest.Say("看到了"), chattest.Say("收到"))
	r, store := newRunner(t, client)
	if _, err := runSync(r, bg, "s", Parts(chat.Text("这是什么"), chat.Media(buf.Bytes()), chat.MediaURL("https://example.com/files/report.pdf?sig=1"))); err != nil {
		t.Fatal(err)
	}
	// Types are not restricted: undetectable content and URLs without an extension are passed through as is.
	if _, err := runSync(r, bg, "any", Parts(chat.Media([]byte{0, 1, 2, 3}), chat.MediaURL("https://example.com/download?id=1"))); err != nil {
		t.Fatalf("the framework should not reject media by type: %v", err)
	}
	if any := client.Requests()[1].Messages[0].Content; len(any) != 2 || any[0].MIMEType != "application/octet-stream" || any[1].URL == "" {
		t.Fatalf("media of unknown type should enter the model context as is: %+v", any)
	}
	sent := client.Requests()[0].Messages[0].Content
	if len(sent) != 3 || sent[0].Text != "这是什么" || sent[1].MIMEType != "image/png" || !bytes.Equal(sent[1].Data, buf.Bytes()) ||
		sent[2].MIMEType != "application/pdf" || sent[2].URL == "" || len(sent[2].Data) != 0 {
		t.Fatalf("media should enter the model context with the user message as is, with its type detected: %+v", sent)
	}
	before := saved(t, store, "s")
	if len(before[0].Content()) != 3 || before[0].Content()[1].Type != chat.ContentTypeMedia || before[0].Text() != "这是什么[media:image/png][media:application/pdf]" {
		t.Fatalf("media should be stored with the user message: %+v", before[0].Content())
	}
	for name, in := range map[string]Input{
		"both data and URL":    Parts(chat.ContentBlock{Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: []byte{1}, URL: "https://example.com/a.png"}),
		"neither data nor URL": Parts(chat.ContentBlock{Type: chat.ContentTypeMedia, MIMEType: "image/png"}),
		"forged tool call":     Parts(chat.ContentBlock{Type: chat.ContentTypeToolCall, ToolCall: &chat.ToolCall{ID: "x", Name: "lookup"}}),
	} {
		if _, err := runSync(r, bg, "s", in); err == nil {
			t.Fatalf("%s: should fail before storing", name)
		}
	}
	before = saved(t, store, "s")
	if _, err := runSync(r, bg, "s", Input{}); err == nil {
		t.Fatal("empty input should fail")
	}
	if len(saved(t, store, "s")) != len(before) {
		t.Fatal("nothing should be stored for invalid input")
	}
}

func TestEventsComeInOrder(t *testing.T) {
	client := newClient(lookupThenAnswer("完成")...)
	r, _ := newRunner(t, client)
	var order []string
	result, err := stream(r, bg, "s", Text("开始"), func(event Event) {
		switch {
		case event.Type == EventMessageSaved && event.Message.Type == session.TypeAssistant:
			order = append(order, "saved")
		case event.Type == EventAgentEvent && event.AgentEvent.Type == agent.EventMessageEnd && event.AgentEvent.Message.Role == chat.RoleAssistant:
			order = append(order, "agent_end")
		case event.Type == EventRunStart || event.Type == EventRunEnd:
			order = append(order, string(event.Type))
		}
		if event.SessionID != "s" || event.RunID == "" {
			t.Errorf("events should carry the session and run IDs: %+v", event)
		}
	})
	if err != nil || result == nil || result.Final.Text() != "完成" {
		t.Fatalf("run_end should carry the run result: %+v %v", result, err)
	}
	if got := strings.Join(order, ","); got != "run_start,saved,agent_end,saved,agent_end,run_end" {
		t.Fatalf("message_saved should come before the corresponding agent event: %s", got)
	}
}

func TestStartFailsAtOnceWhenBusyAndWaitReportsRunError(t *testing.T) {
	client := newGatedClient(chattest.Say("回答"), chattest.Fail(&chat.Error{Kind: chat.KindServer, Message: "过载"}))
	r, _ := newRunner(t, client)
	done := runAsync(r, "s", "开始")
	<-client.started
	if run, err := r.Start(bg, "s", Text("挤进来")); !errors.Is(err, ErrBusy) || run != nil {
		t.Fatalf("Start should fail at once while the session is busy: %v", err)
	}
	close(client.release)
	<-done

	result, err := stream(r, bg, "s", Text("再来"), nil)
	var ce *chat.Error
	if !errors.As(err, &ce) || result == nil || result.Status != agent.StatusFailed {
		t.Fatalf("Wait should give both the result and the error of a failed run: %+v %v", result, err)
	}
}

func TestRunOutlivesItsCallerAndStopsOnAbort(t *testing.T) {
	client := newGatedClient(lookupThenAnswer("完成")...)
	r, store := newRunner(t, client)
	ctx, cancel := context.WithCancel(bg)
	run, err := r.Start(ctx, "s", Text("开始"))
	if err != nil {
		t.Fatal(err)
	}
	<-client.started
	cancel() // the caller goes away, the run goes on
	close(client.release)
	result, err := run.Wait(bg)
	if err != nil || result.Final.Text() != "完成" || result.SessionID != run.SessionID() || result.RunID != run.ID() {
		t.Fatalf("the run should finish after its caller's ctx is canceled: %+v %v", result, err)
	}
	if got := sessionRoles(saved(t, store, "s")); got != "user,assistant,tool,assistant" {
		t.Fatalf("every message of the run should be stored: %s", got)
	}

	gated := newGatedClient(chattest.Say("不应到达"))
	r, _ = newRunner(t, gated)
	run, _ = r.Start(bg, "s", Text("开始"))
	<-gated.started
	r.Abort("s")
	result, err = run.Wait(bg)
	if !errors.Is(err, context.Canceled) || result.Status != agent.StatusAborted || r.Running("s") {
		t.Fatalf("Abort should stop the run, and the session should be free once Wait returns: %+v %v", result, err)
	}
}

func TestEventsAndSubscribeReplayToEveryReceiver(t *testing.T) {
	reply := func(_ context.Context, b *chat.Builder) {
		b.TextDelta("你")
		b.TextDelta("好")
		b.Done(chat.StopReasonStop)
	}
	client := newGatedClient(reply, chattest.Say("再见"))
	r, _ := newRunner(t, client)
	run, err := r.Start(bg, "s", Text("开始"))
	if err != nil {
		t.Fatal(err)
	}
	<-client.started
	// A view attaching mid-run gets the run from its start, then the later runs of the session.
	ctx, cancel := context.WithCancel(bg)
	followed := make(chan Event, 100)
	go func() {
		for event := range r.Subscribe(ctx, "s") {
			followed <- event
		}
		close(followed)
	}()
	attached := <-followed // make sure the view attached before the run ends
	close(client.release)

	var live, replayed []Event
	for event := range run.Events(bg) {
		live = append(live, event)
	}
	for event := range run.Events(bg) {
		replayed = append(replayed, event)
	}
	if len(live) == 0 || len(replayed) != len(live) || replayed[0].Type != EventRunStart || replayed[len(replayed)-1].Type != EventRunEnd {
		t.Fatalf("every receiver should get the whole run: live=%d replayed=%d", len(live), len(replayed))
	}
	var text string
	var updates, snapshots int
	for _, event := range replayed {
		text += event.TextDelta()
		if ae := event.AgentEvent; ae != nil && ae.Type == agent.EventMessageUpdate {
			updates++
			if ae.Message != nil {
				snapshots++
			}
		}
	}
	if text != "你好" || updates < 2 || snapshots != 1 {
		t.Fatalf("a replay should keep every delta and only the latest snapshot: %q updates=%d snapshots=%d", text, updates, snapshots)
	}

	next := func(events ...Event) []Event {
		for event := range followed {
			events = append(events, event)
			if event.Type == EventRunEnd {
				break
			}
		}
		return events
	}
	if first := next(attached); len(first) != len(live) || first[0].RunID != run.ID() {
		t.Fatalf("Subscribe should replay the run in progress from its start: %d events", len(first))
	}
	second, err := stream(r, bg, "s", Text("再来"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if events := next(); len(events) == 0 || events[0].Type != EventRunStart || events[0].RunID != second.RunID {
		t.Fatalf("Subscribe should follow the next run of the session: %+v", events)
	}
	cancel()
	for range followed {
	}
	r.mu.Lock()
	waiting := len(r.waiters)
	r.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("a canceled Subscribe should stop waiting: %d", waiting)
	}
}

func TestWaitReturnsAfterRun(t *testing.T) {
	client := newGatedClient(chattest.Say("完成"))
	r, _ := newRunner(t, client)
	done := runAsync(r, "s", "开始")
	<-client.started
	waited := make(chan error, 1)
	go func() { waited <- r.Wait(bg, "s") }()
	select {
	case <-waited:
		t.Fatal("Wait should not return before the run ends")
	case <-time.After(20 * time.Millisecond):
	}
	close(client.release)
	<-done
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after the run ended")
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("a missing Agent should fail: %v", err)
	}
}

func TestAgentModelIsOptionalWhenRunsPassOne(t *testing.T) {
	r, store := newRunner(t, nil)
	if _, err := r.Start(bg, "s", Text("你好")); !errors.Is(err, errNoModel) {
		t.Fatalf("a run without any model should fail at once: %v", err)
	}
	if _, _, err := r.Compact(bg, "s", CompactOptions{}); !errors.Is(err, errNoModel) {
		t.Fatalf("a compaction without any model should fail at once: %v", err)
	}
	if msgs, _ := store.ListMessages(bg, "s"); len(msgs) != 0 {
		t.Fatal("nothing should be stored when there is no model")
	}
	if _, err := runSync(r, bg, "s", Text("你好"), WithModel(newClient(chattest.Say("好")))); err != nil {
		t.Fatal(err)
	}
}

type assistantConverter struct{ DefaultConverter }

func (c assistantConverter) ToChatMessages(ctx context.Context, messages []session.Message) ([]chat.Message, error) {
	out, err := c.DefaultConverter.ToChatMessages(ctx, messages)
	// Only change queued messages: converted on their own they are a single user message.
	if err == nil && len(out) == 1 && out[0].TextContent() == "转向" {
		out[0].Role = chat.RoleAssistant
	}
	return out, err
}

func TestQueuedMessagesMustConvertToUserMessages(t *testing.T) {
	client := newGatedClient(lookupThenAnswer("不应到达")...)
	r, _ := newRunner(t, client, func(c *Config) { c.Converter = assistantConverter{} })
	done := runAsync(r, "s", "开始")
	<-client.started
	_ = r.Steer("s", Text("转向"))
	close(client.release)
	if out := <-done; out.err == nil || !strings.Contains(out.err.Error(), "only user messages are allowed") {
		t.Fatalf("should fail when the converter turns queued messages into another role: %v", out.err)
	}
}

func TestEventTextDelta(t *testing.T) {
	r, _ := newRunner(t, newClient(chattest.Say("你好")))
	var text string
	if _, err := stream(r, bg, "s", Text("hi"), func(event Event) { text += event.TextDelta() + event.ThinkingDelta() }); err != nil {
		t.Fatal(err)
	}
	if text != "你好" {
		t.Fatalf("should get the text delta directly from the event: %q", text)
	}
}

// taskRecorder collects background task callbacks.
type taskRecorder struct {
	mu       sync.Mutex
	calls    []string
	output   string
	finished chan finishedTask
}

type finishedTask struct {
	task     tool.Task
	delivery TaskResultDelivery
}

func newTaskRecorder() *taskRecorder {
	return &taskRecorder{finished: make(chan finishedTask, 4)}
}

func (rec *taskRecorder) install(c *Config) {
	c.OnTaskStarted = func(context.Context, tool.Task) { rec.add("started") }
	c.OnTaskProgressed = func(_ context.Context, _ tool.Task, u tool.TaskUpdate) {
		rec.add("progressed")
		rec.mu.Lock()
		rec.output += u.Output
		rec.mu.Unlock()
	}
	c.OnTaskFinished = func(_ context.Context, task tool.Task, delivery TaskResultDelivery) {
		rec.add("finished")
		rec.finished <- finishedTask{task, delivery}
	}
}

func (rec *taskRecorder) add(call string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if n := len(rec.calls); n > 0 && rec.calls[n-1] == call {
		return
	}
	rec.calls = append(rec.calls, call)
}

func (rec *taskRecorder) order() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return strings.Join(rec.calls, ",")
}

func (rec *taskRecorder) wait(t *testing.T) finishedTask {
	t.Helper()
	select {
	case f := <-rec.finished:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("the task did not end")
		return finishedTask{}
	}
}

func subAgentCall(id, args string) chat.ToolCall {
	return chat.ToolCall{ID: id, Name: agent.SubAgentToolName, Arguments: []byte(args)}
}

// newSubAgentRunner creates a Runner whose main Agent can hand tasks to the specialized sub-agent worker and manage them with the task tool.
// worker also has the sub-agent and task tools attached, to confirm that sub-agents cannot spawn background tasks.
func newSubAgentRunner(t *testing.T, mainClient, workerClient chat.Client, rec *taskRecorder) (*Runner, *session.MemoryStore) {
	worker := &agent.Agent{Name: "worker", Description: "执行任务", Model: workerClient,
		Tools: []tool.Tool{lookupTool(), agent.SubAgentTool(agent.SubAgentConfig{}), tasktool.Tool()}}
	return newRunner(t, mainClient, func(c *Config) {
		c.Agent.Tools = append(c.Agent.Tools, agent.SubAgentTool(agent.SubAgentConfig{Agents: []*agent.Agent{worker}}), tasktool.Tool())
		rec.install(c)
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSubAgentRunsInBackground(t *testing.T) {
	mainClient := newClient(
		chattest.CallTools(subAgentCall("c1", `{"agent":"worker","task":"查一下工具结果","description":"查工具"}`)),
		chattest.Say("已经开始了"),
		chattest.Say("结果是工具结果"),
	)
	workerClient := newGatedClient(chattest.CallTools(chat.ToolCall{ID: "w1", Name: "lookup"}), chattest.Say("子任务的结论"))
	rec := newTaskRecorder()
	r, store := newSubAgentRunner(t, mainClient, workerClient, rec)

	// The tool returns immediately: when the main run ends, the sub-task is still stuck in its first model call.
	result, err := runSync(r, bg, "parent", Text("开始"))
	if err != nil || result.Final.Text() != "已经开始了" {
		t.Fatalf("the main run should not wait for the sub-task: %+v %v", result, err)
	}
	running := r.ListTasks("parent")
	if len(running) != 1 || running[0].Status != tool.TaskRunning || running[0].Type != agent.SubAgentTaskType ||
		running[0].Description != "查工具" || running[0].Details.(agent.SubAgentDetails).Agent != "worker" {
		t.Fatalf("should list the running sub-task: %+v", running)
	}

	close(workerClient.release)
	f := rec.wait(t)
	d := f.task.Details.(agent.SubAgentDetails)
	if f.delivery != ResultSavedToHistory || f.task.Status != tool.TaskCompleted || f.task.Result != "子任务的结论" || d.Turns != 2 || d.LastTool != "lookup" {
		t.Fatalf("the result should be written to history while the parent session is idle: %+v", f)
	}
	if got := rec.order(); got != "started,progressed,finished" {
		t.Fatalf("wrong callback order: %s", got)
	}
	if len(r.ListTasks("parent")) != 0 {
		t.Fatal("the task should no longer be listed after its result is delivered")
	}

	parent := saved(t, store, "parent")
	notice := parent[len(parent)-1]
	if got := sessionRoles(parent); got != "user,assistant,tool,assistant,user" {
		t.Fatalf("the parent session should end with a result notice: %s", got)
	}
	if notice.Kind != session.KindTaskResult || notice.Visible || notice.Source != session.SourceSystem ||
		!strings.Contains(notice.Text(), `type="sub_agent" status="completed" description="查工具"`) || !strings.Contains(notice.Text(), "子任务的结论") {
		t.Fatalf("wrong result notice: %+v %q", notice, notice.Text())
	}
	if task, ok := notice.Details.(tool.Task); !ok || task.ID != f.task.ID {
		t.Fatalf("the result notice should carry the task snapshot: %+v", notice.Details)
	}
	if notice.RunID != "" {
		t.Fatalf("a result notice written to history while idle belongs to no run: %q", notice.RunID)
	}

	// The full sub-task is stored in a child session of its own (the session ID is the task ID), which records its parent.
	child := saved(t, store, f.task.ID)
	if got := sessionRoles(child); got != "user,assistant,tool,assistant" || child[0].Text() != "查一下工具结果" {
		t.Fatalf("the child session should hold every message of the sub-task: %s", got)
	}
	if s, err := store.GetSession(bg, f.task.ID); err != nil || s.ParentID != "parent" {
		t.Fatalf("the child session should record its parent: %+v %v", s, err)
	}
	if tools := workerClient.Requests()[0].Tools; len(tools) != 1 || tools[0].Name != "lookup" {
		t.Fatalf("sub-agents should not get tools that need background tasks: %+v", tools)
	}

	// Empty input continues from history: the main Agent handles the result written to history.
	result, err = runSync(r, bg, "parent", Input{})
	if err != nil || result.Final.Text() != "结果是工具结果" {
		t.Fatalf("empty input should continue the run from history: %+v %v", result, err)
	}
	if msgs := mainClient.Requests()[2].Messages; !strings.Contains(msgs[len(msgs)-1].TextContent(), "<task_result") {
		t.Fatalf("the woken model should see the result notice: %+v", msgs[len(msgs)-1])
	}
	if _, err := runSync(r, bg, "parent", Input{}); err == nil {
		t.Fatal("empty input should fail when history ends with an assistant message")
	}
}

func TestGeneralSubAgentUsesTheRunModel(t *testing.T) {
	base := newClient()
	other := newClient(chattest.CallTools(subAgentCall("c1", `{"task":"查一下","description":"查"}`)), chattest.Say("好"), chattest.Say("好"))
	rec := newTaskRecorder()
	r, _ := newRunner(t, base, func(c *Config) {
		c.Agent.Tools = append(c.Agent.Tools, agent.SubAgentTool(agent.SubAgentConfig{}), tasktool.Tool())
		rec.install(c)
	})
	if _, err := runSync(r, bg, "p", Text("开始"), WithModel(other)); err != nil {
		t.Fatal(err)
	}
	if f := rec.wait(t); f.task.Status != tool.TaskCompleted || len(other.Requests()) != 3 || len(base.Requests()) != 0 {
		t.Fatalf("the general sub-agent should run on the run's model: %+v requests=%d/%d", f.task, len(other.Requests()), len(base.Requests()))
	}
}

func TestEnvironmentMiddlewaresApplyToSubAgents(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	record := func(layer string) agent.Middleware {
		return agent.Middleware{WrapTool: func(ctx context.Context, inv *agent.ToolInvocation, next agent.ToolNext) (*tool.Result, error) {
			mu.Lock()
			calls = append(calls, fmt.Sprintf("%s:%s@%d:%s", layer, inv.Call.Name, inv.Depth, inv.AgentName))
			mu.Unlock()
			return next(ctx, inv), nil
		}}
	}
	worker := &agent.Agent{Name: "worker", Description: "执行任务", Middlewares: []agent.Middleware{record("worker")},
		Model: newClient(chattest.CallTools(chat.ToolCall{ID: "w1", Name: "lookup"}), chattest.Say("结论")),
		Tools: []tool.Tool{lookupTool()}}
	mainClient := newClient(chattest.CallTools(subAgentCall("c1", `{"agent":"worker","task":"查"}`)), chattest.Say("已开始"))
	rec := newTaskRecorder()
	r, _ := newRunner(t, mainClient, func(c *Config) {
		c.Agent.Name = "main"
		c.Agent.Tools = append(c.Agent.Tools, agent.SubAgentTool(agent.SubAgentConfig{Agents: []*agent.Agent{worker}}), tasktool.Tool())
		c.Agent.Middlewares = []agent.Middleware{record("main")}
		c.Middlewares = []agent.Middleware{record("env")}
		rec.install(c)
	})
	if _, err := runSync(r, bg, "s", Text("开始")); err != nil {
		t.Fatal(err)
	}
	if f := rec.wait(t); f.task.Status != tool.TaskCompleted {
		t.Fatalf("the sub-task should complete: %+v", f.task)
	}
	mu.Lock()
	got := strings.Join(calls, ",")
	mu.Unlock()
	// Runtime middlewares are outermost and apply to the main Agent and sub-agents; an Agent's own middlewares only apply to itself.
	if want := "env:sub_agent@0:main,main:sub_agent@0:main,env:lookup@1:worker,worker:lookup@1:worker"; got != want {
		t.Fatalf("wrong middleware scope:\n got %s\nwant %s", got, want)
	}
}

func TestSubAgentResultIsInjectedIntoRunningRun(t *testing.T) {
	mainStarted, mainRelease := make(chan struct{}, 1), make(chan struct{})
	mainClient := newClient(
		chattest.CallTools(subAgentCall("c1", `{"agent":"worker","task":"做点事"}`)),
		chattest.Gate(mainStarted, mainRelease, chattest.CallTools(chat.ToolCall{ID: "m1", Name: "lookup"})),
		chattest.Say("看到了结果"),
	)
	rec := newTaskRecorder()
	r, store := newSubAgentRunner(t, mainClient, newClient(chattest.Say("子任务的结论")), rec)

	done := make(chan error, 1)
	go func() {
		_, err := runSync(r, bg, "parent", Text("开始"))
		done <- err
	}()
	<-mainStarted
	// The sub-task ends during the main run's second model call: its result is queued as a notice and injected after this turn's tools finish.
	waitFor(t, "结果通知排队", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		h := r.active["parent"]
		return h != nil && len(h.notices) == 1
	})
	close(mainRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f := rec.wait(t)
	if f.delivery != ResultInjectedIntoRun || f.task.Status != tool.TaskCompleted {
		t.Fatalf("the result should be injected into the run while the parent session is running: %+v", f)
	}
	if msgs := mainClient.Requests()[2].Messages; !strings.Contains(msgs[len(msgs)-1].TextContent(), "子任务的结论") {
		t.Fatalf("the next model call should see the result notice: %+v", msgs[len(msgs)-1])
	}
	parent := saved(t, store, "parent")
	if got := sessionRoles(parent); got != "user,assistant,tool,assistant,tool,user,assistant" {
		t.Fatalf("the result notice should be stored when injected: %s", got)
	}
	if notice := parent[5]; notice.Kind != session.KindTaskResult || notice.RunID == "" || notice.RunID != parent[0].RunID {
		t.Fatalf("an injected result notice should belong to the run that injected it: %+v", notice)
	}
}

func TestSubAgentOutlivesAbortAndCanBeStopped(t *testing.T) {
	mainStarted := make(chan struct{}, 1)
	mainClient := newClient(
		chattest.CallTools(subAgentCall("c1", `{"agent":"worker","task":"很慢的任务"}`)),
		chattest.Gate(mainStarted, make(chan struct{}), chattest.Say("不会走到这里")),
	)
	workerClient := newGatedClient(chattest.Say("不会完成"))
	rec := newTaskRecorder()
	r, store := newSubAgentRunner(t, mainClient, workerClient, rec)

	done := make(chan error, 1)
	go func() {
		_, err := runSync(r, bg, "parent", Text("开始"))
		done <- err
	}()
	<-mainStarted
	r.Abort("parent")
	if err := <-done; err == nil {
		t.Fatal("the main run should be canceled")
	}
	<-workerClient.started
	tasks := r.ListTasks("parent")
	if len(tasks) != 1 || tasks[0].Status != tool.TaskRunning {
		t.Fatalf("canceling the main run should not cancel background tasks: %+v", tasks)
	}
	id := tasks[0].ID

	other := sessionTaskManager{r, "other"}
	if _, err := other.Stop(bg, id); !errors.Is(err, tool.ErrTaskNotFound) {
		t.Fatalf("should not stop tasks spawned by other sessions: %v", err)
	}
	if _, err := other.Get(bg, id); !errors.Is(err, tool.ErrTaskNotFound) || len(other.List(bg)) != 0 {
		t.Fatalf("should not see tasks spawned by other sessions: %v", err)
	}
	if err := r.StopTask(bg, id); err != nil {
		t.Fatal(err)
	}
	f := rec.wait(t)
	if f.task.Status != tool.TaskStopped || f.delivery != ResultSavedToHistory {
		t.Fatalf("a stopped task should also send its result back to the parent session: %+v", f)
	}
	parent := saved(t, store, "parent")
	if last := parent[len(parent)-1]; !strings.Contains(last.Text(), `status="stopped"`) {
		t.Fatalf("the parent session should receive the stop notice: %q", last.Text())
	}
	if err := r.StopTask(bg, id); !errors.Is(err, tool.ErrTaskNotFound) {
		t.Fatalf("an ended task should be reported as not found: %v", err)
	}
}

func TestTaskToolStatusAndStop(t *testing.T) {
	var r *Runner
	// The second turn looks up the running task by ID and stops it: stop waits in the tool for the task to end, and the result is then injected into this run.
	statusAndStop := func(_ context.Context, b *chat.Builder) {
		id := r.ListTasks("parent")[0].ID
		b.ToolCall("s1", tasktool.ToolName, []byte(`{"action":"status","id":"`+id+`"}`))
		b.ToolCall("s2", tasktool.ToolName, []byte(`{"action":"stop","id":"`+id+`"}`))
		b.Done(chat.StopReasonToolUse)
	}
	mainClient := newClient(
		chattest.CallTools(subAgentCall("c1", `{"agent":"worker","task":"很慢的任务"}`)),
		statusAndStop,
		chattest.Say("已经停下了"),
	)
	workerClient := newGatedClient(chattest.Say("不会完成"))
	rec := newTaskRecorder()
	r, _ = newSubAgentRunner(t, mainClient, workerClient, rec)
	r.agent.SequentialTools = true

	result, err := runSync(r, bg, "parent", Text("开始"))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, msg := range result.Messages {
		if msg.Type == session.TypeTool {
			texts = append(texts, msg.Text())
		}
	}
	if len(texts) != 3 || !strings.Contains(texts[1], `"status":"running"`) || !strings.Contains(texts[2], `"status":"stopped"`) {
		t.Fatalf("wrong status and stop results: %v", texts)
	}
	f := rec.wait(t)
	if f.task.Status != tool.TaskStopped || f.delivery != ResultInjectedIntoRun {
		t.Fatalf("the result of a task stopped by the model should be injected into the current run: %+v", f)
	}
}

func TestFailedSubAgentIsStillRecorded(t *testing.T) {
	mainClient := newClient(
		chattest.CallTools(subAgentCall("c1", `{"agent":"worker","task":"会失败的任务"}`)),
		chattest.Say("已经开始了"),
	)
	workerClient := newClient(func(_ context.Context, b *chat.Builder) {
		b.TextDelta("做到一半")
		b.Fail(&chat.Error{Kind: chat.KindServer, Message: "子模型过载"})
	})
	rec := newTaskRecorder()
	r, store := newSubAgentRunner(t, mainClient, workerClient, rec)
	if _, err := runSync(r, bg, "parent", Text("开始")); err != nil {
		t.Fatal(err)
	}
	f := rec.wait(t)
	if f.task.Status != tool.TaskFailed || !strings.Contains(f.task.Error, "子模型过载") || f.task.Result != "做到一半" {
		t.Fatalf("a failed sub-task should record the reason and the output before the interruption: %+v", f.task)
	}
	parent := saved(t, store, "parent")
	if last := parent[len(parent)-1]; !strings.Contains(last.Text(), `status="failed"`) || !strings.Contains(last.Text(), "error: ") {
		t.Fatalf("the parent session should receive the failure notice: %q", last.Text())
	}
	child := saved(t, store, f.task.ID)
	if got := sessionRoles(child); got != "user,assistant" || child[1].ChatMessage.StopReason != chat.StopReasonError {
		t.Fatalf("a failed sub-task should also leave a record for auditing: %s", got)
	}
}

// gatedExecutor prints one line, waits for release to close, then prints another line and exits successfully.
type gatedExecutor struct{ release chan struct{} }

func (e gatedExecutor) Run(ctx context.Context, command string, out io.Writer) (int, error) {
	fmt.Fprintf(out, "%s started\n", command)
	select {
	case <-e.release:
	case <-ctx.Done():
		return -1, ctx.Err()
	}
	fmt.Fprintln(out, "done")
	return 0, nil
}

func TestBackgroundShellCommand(t *testing.T) {
	mainClient := newClient(
		chattest.CallTools(chat.ToolCall{ID: "c1", Name: shell.ToolName, Arguments: []byte(`{"command":"build","description":"构建","run_in_background":true}`)}),
		chattest.Say("构建已经开始"),
	)
	exec := gatedExecutor{release: make(chan struct{})}
	rec := newTaskRecorder()
	r, store := newRunner(t, mainClient, func(c *Config) {
		c.Agent.Tools = append(c.Agent.Tools, shell.Tool(shell.Config{Executor: exec}), tasktool.Tool())
		rec.install(c)
	})
	if _, err := runSync(r, bg, "s", Text("构建一下")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "命令开始输出", func() bool {
		tasks := r.ListTasks("s")
		return len(tasks) == 1 && tasks[0].Output == "build started\n"
	})
	if tasks := r.ListTasks("s"); tasks[0].Type != shell.TaskType || tasks[0].Description != "构建" {
		t.Fatalf("a background command should be registered as a shell task: %+v", tasks[0])
	}

	close(exec.release)
	f := rec.wait(t)
	if f.task.Status != tool.TaskCompleted || f.delivery != ResultSavedToHistory || f.task.Result != "build started\ndone\n[exit code: 0]" ||
		f.task.Details.(shell.Details).ExitCode != 0 || rec.output != "build started\ndone\n" {
		t.Fatalf("after the command ends its result should come back to the session, with the progress callback receiving all output: %+v %q", f, rec.output)
	}
	messages := saved(t, store, "s")
	if last := messages[len(messages)-1]; last.Kind != session.KindTaskResult || !strings.Contains(last.Text(), `type="shell" status="completed"`) {
		t.Fatalf("the session should receive the command's result notice: %q", last.Text())
	}
}
