package runner

import (
	"context"
	"errors"
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
)

// writeThenWait writes part of a reply, signals streaming, and waits until the call is canceled.
func writeThenWait(streaming chan<- struct{}) chattest.Script {
	return func(ctx context.Context, b *chat.Builder) {
		b.TextDelta("写到一半")
		close(streaming)
		<-ctx.Done()
		b.Fail(chat.WrapError(ctx, ctx.Err(), 0, 0))
	}
}

func TestShutdownStopsRunsAndTasks(t *testing.T) {
	gates := commandGates{"build": make(chan struct{})}
	client := newClient(chattest.CallTools(backgroundCall("c1", "build")), chattest.Say("已开始"))
	rec := newTaskRecorder()
	r, store := newRunner(t, client, func(c *Config) {
		c.Agent.Tools = append(c.Agent.Tools, shell.Tool(shell.Config{Executor: gates}))
		rec.install(c)
		c.TaskWake = func(context.Context, tool.Task) ([]RunOption, bool) { return nil, true }
	})
	if _, err := runSync(r, bg, "idle", Text("构建一下")); err != nil {
		t.Fatal(err)
	}

	// A subscriber of the idle session, which should see the stopped task's result written during the shutdown.
	var seen []session.Message
	subscribed := make(chan struct{})
	go func() {
		defer close(subscribed)
		for event := range r.Subscribe(bg, "idle") {
			if event.Type == EventMessageSaved {
				seen = append(seen, *event.Message)
			}
		}
	}()
	waitFor(t, "the subscriber to attach", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.followers["idle"] != nil
	})

	streaming := make(chan struct{})
	run, err := r.Start(bg, "busy", Text("写点什么"), WithModel(chattest.NewClient(writeThenWait(streaming))))
	if err != nil {
		t.Fatal(err)
	}
	<-streaming
	if err := run.FollowUp(Text("还有一个问题")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	result, _ := run.Wait(bg)
	if result.Status != agent.StatusAborted || len(result.Pending.FollowUp) != 1 {
		t.Fatalf("the run should be aborted, keeping its queued follow-up in Pending: %+v", result)
	}
	busy := saved(t, store, "busy")
	if last := busy[len(busy)-1]; last.ChatMessage.StopReason != chat.StopReasonAborted || last.Text() != "写到一半" {
		t.Fatalf("what the model had written should be stored as an aborted message: %+v", last)
	}

	f := rec.wait(t)
	if f.task.Status != tool.TaskStopped || f.task.Error != shutdownReason || f.delivery != ResultSavedToHistory {
		t.Fatalf("the task should be stopped with the reason and its result written without waking: %+v", f)
	}
	idle := saved(t, store, "idle")
	if last := idle[len(idle)-1]; last.Kind != session.KindTaskResult || !strings.Contains(last.Text(), shutdownReason) {
		t.Fatalf("the idle session should receive the stopped result: %q", last.Text())
	}

	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe should end once the shutdown has finished")
	}
	if len(seen) != 1 || seen[0].Kind != session.KindTaskResult {
		t.Fatalf("the subscriber should see the result written during the shutdown: %+v", seen)
	}

	if _, err := r.Start(bg, "idle", Text("还在吗")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Shutdown should get ErrClosed: %v", err)
	}
	if _, _, err := r.Compact(bg, "idle", CompactOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Compact after Shutdown should get ErrClosed: %v", err)
	}
	if _, err := (sessionTaskManager{r, "idle"}).Start(bg, tool.TaskSpec{Run: func(context.Context, func(tool.TaskUpdate)) (tool.TaskResult, error) {
		return tool.TaskResult{}, nil
	}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("starting a task after Shutdown should get ErrClosed: %v", err)
	}
	if _, err := r.DeliverTaskResult(bg, tool.Task{ID: "task_1", SessionID: "idle", Status: tool.TaskFailed}); !errors.Is(err, ErrClosed) {
		t.Fatalf("DeliverTaskResult after Shutdown should get ErrClosed: %v", err)
	}
	if err := r.Shutdown(bg); err != nil {
		t.Fatalf("calling Shutdown again should be safe: %v", err)
	}
}

func TestShutdownReturnsWhenCtxEnds(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	// A model call that ignores cancellation until released.
	stubborn := chattest.NewClient(func(ctx context.Context, b *chat.Builder) {
		close(started)
		<-release
		b.Fail(chat.WrapError(ctx, ctx.Err(), 0, 0))
	})
	r, _ := newRunner(t, stubborn)
	run, err := r.Start(bg, "s", Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	<-started

	ctx, cancel := context.WithTimeout(bg, 20*time.Millisecond)
	defer cancel()
	if err := r.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown should give up when ctx ends: %v", err)
	}
	close(release)
	if err := r.Shutdown(bg); err != nil {
		t.Fatal(err)
	}
	if result, _ := run.Wait(bg); result.Status != agent.StatusAborted {
		t.Fatalf("the run should end aborted: %+v", result)
	}
}

func TestShutdownRacesTaskStarts(t *testing.T) {
	r, _ := newRunner(t, newClient(chattest.Say("你好")))
	if _, err := runSync(r, bg, "s", Text("hi")); err != nil {
		t.Fatal(err)
	}
	tasks := sessionTaskManager{r, "s"}
	spec := tool.TaskSpec{Type: "wait", Run: func(ctx context.Context, _ func(tool.TaskUpdate)) (tool.TaskResult, error) {
		<-ctx.Done()
		return tool.TaskResult{}, ctx.Err()
	}}

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := tasks.Start(bg, spec); err != nil && !errors.Is(err, ErrClosed) {
				t.Error(err)
			}
		})
	}
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if left := r.ListTasks("s"); len(left) != 0 {
		t.Fatalf("every task started before the shutdown should be stopped and delivered: %+v", left)
	}
}

// cancelSensitiveStore fails EnsureSession when its ctx is canceled, as a database store would; entered and release let a test cancel the run in between.
type cancelSensitiveStore struct {
	*session.MemoryStore
	entered chan struct{}
	release chan struct{}
}

func (s *cancelSensitiveStore) EnsureSession(ctx context.Context, sess session.Session) error {
	close(s.entered)
	<-s.release
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.MemoryStore.EnsureSession(ctx, sess)
}

func TestShutdownRightAfterStartKeepsTheInput(t *testing.T) {
	store := &cancelSensitiveStore{MemoryStore: session.NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	r, _ := newRunner(t, newClient(chattest.Say("你好")), func(c *Config) { c.Store = store })
	run, err := r.Start(bg, "s", Text("刚发出的问题"))
	if err != nil {
		t.Fatal(err)
	}
	<-store.entered
	ctx, cancel := context.WithTimeout(bg, 20*time.Millisecond)
	defer cancel()
	_ = r.Shutdown(ctx) // cancels the run while the session is being created
	close(store.release)
	if err := r.Shutdown(bg); err != nil {
		t.Fatal(err)
	}
	run.Wait(bg)
	if messages := saved(t, store.MemoryStore, "s"); len(messages) == 0 || messages[0].Text() != "刚发出的问题" {
		t.Fatalf("a run aborted at once should still store its input: %+v", messages)
	}
}

func TestShutdownAbortsASubAgent(t *testing.T) {
	streaming := make(chan struct{})
	mainClient := newClient(
		chattest.CallTools(subAgentCall("c1", `{"agent":"worker","task":"查一下","description":"查"}`)),
		chattest.Say("已派出"),
	)
	rec := newTaskRecorder()
	r, store := newSubAgentRunner(t, mainClient, chattest.NewClient(writeThenWait(streaming)), rec)
	if _, err := runSync(r, bg, "s", Text("派个子任务")); err != nil {
		t.Fatal(err)
	}
	<-streaming

	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	f := rec.wait(t)
	if f.task.Status != tool.TaskStopped || f.task.Error != shutdownReason {
		t.Fatalf("the sub-agent task should be stopped with the reason: %+v", f.task)
	}
	child := saved(t, store, f.task.ID)
	if last := child[len(child)-1]; last.ChatMessage.StopReason != chat.StopReasonAborted || last.Text() != "写到一半" {
		t.Fatalf("the child session's run should be aborted and keep what it wrote: %+v", last)
	}
	parent := saved(t, store, "s")
	if last := parent[len(parent)-1]; last.Kind != session.KindTaskResult || !strings.Contains(last.Text(), `status="stopped"`) {
		t.Fatalf("the parent session should receive the stopped result: %q", last.Text())
	}
}

func TestShutdownAbortsACompaction(t *testing.T) {
	summarizing := make(chan struct{})
	client := newClient(chattest.Say("回答一"), chattest.Say("回答二"), func(ctx context.Context, b *chat.Builder) {
		close(summarizing)
		<-ctx.Done()
		b.Fail(chat.WrapError(ctx, ctx.Err(), 0, 0))
	})
	client.Model.MaxInputTokens = 100000
	r, store := newRunner(t, client)
	for _, q := range []string{"问题一", "问题二"} {
		if _, err := runSync(r, bg, "c", Text(long(q))); err != nil {
			t.Fatal(err)
		}
	}
	before := len(saved(t, store, "c"))

	compacted := make(chan error, 1)
	go func() {
		_, _, err := r.Compact(bg, "c", CompactOptions{})
		compacted <- err
	}()
	<-summarizing
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-compacted; err == nil {
		t.Fatal("a compaction cut off by Shutdown should fail")
	}
	if after := saved(t, store, "c"); len(after) != before {
		t.Fatalf("an aborted compaction should store no summary: %s", sessionRoles(after))
	}
}
