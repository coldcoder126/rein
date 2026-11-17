package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
	"github.com/coldcoder126/rein/session"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/shell"
)

// commandGates runs each command until its own channel is closed.
type commandGates map[string]chan struct{}

func (g commandGates) Run(ctx context.Context, command string, out io.Writer) (int, error) {
	select {
	case <-g[command]:
	case <-ctx.Done():
		return -1, ctx.Err()
	}
	fmt.Fprintf(out, "%s done\n", command)
	return 0, nil
}

func backgroundCall(id, command string) chat.ToolCall {
	return chat.ToolCall{ID: id, Name: shell.ToolName,
		Arguments: fmt.Appendf(nil, `{"command":%q,"run_in_background":true}`, command)}
}

// lastRequestMessage returns the last message of the client's latest request.
func lastRequestMessage(t *testing.T, c *scriptedClient) chat.Message {
	t.Helper()
	requests := c.Requests()
	if len(requests) == 0 {
		t.Fatal("the model was not called")
	}
	msgs := requests[len(requests)-1].Messages
	return msgs[len(msgs)-1]
}

func waitIdle(t *testing.T, r *Runner, sessionID string) {
	t.Helper()
	waitFor(t, "the session to become idle", func() bool { return r.Current(sessionID) == nil })
}

func TestTaskWakeStartsARunWithTheResultAsInput(t *testing.T) {
	gates := commandGates{"build": make(chan struct{})}
	mainClient := newClient(chattest.CallTools(backgroundCall("c1", "build")), chattest.Say("构建已经开始"))
	wakeClient := newClient(chattest.Say("构建完成了"))
	rec := newTaskRecorder()
	var woken tool.Task
	r, store := newRunner(t, mainClient, func(c *Config) {
		c.Agent.Tools = append(c.Agent.Tools, shell.Tool(shell.Config{Executor: gates}))
		rec.install(c)
		c.TaskWake = func(_ context.Context, task tool.Task) ([]RunOption, bool) {
			woken = task
			return []RunOption{WithModel(wakeClient)}, true
		}
	})
	if _, err := runSync(r, bg, "s", Text("构建一下")); err != nil {
		t.Fatal(err)
	}

	close(gates["build"])
	if f := rec.wait(t); f.delivery != ResultStartedRun || woken.ID != f.task.ID {
		t.Fatalf("a result reaching an idle session should start a run: %+v", f)
	}
	waitIdle(t, r, "s")
	if last := lastRequestMessage(t, wakeClient); last.Role != chat.RoleUser || !strings.Contains(last.TextContent(), `status="completed"`) {
		t.Fatalf("the woken run should see the task result as its input, on the model TaskWake chose: %+v", last)
	}
	messages := saved(t, store, "s")
	n := len(messages)
	if messages[n-2].Kind != session.KindTaskResult || messages[n-1].Text() != "构建完成了" {
		t.Fatalf("history should end with the result and the woken reply: %s", sessionRoles(messages))
	}
}

func TestTaskWakeInjectsLaterResultsIntoTheWokenRun(t *testing.T) {
	gates := commandGates{"a": make(chan struct{}), "b": make(chan struct{})}
	mainClient := newClient(chattest.CallTools(backgroundCall("c1", "a"), backgroundCall("c2", "b")), chattest.Say("都开始了"))
	// The woken run's first call waits until b has ended, so b's result arrives while that run is in progress.
	wakeClient := newGatedClient(chattest.Say("a 完成了"), chattest.Say("b 也完成了"))
	rec := newTaskRecorder()
	r, store := newRunner(t, mainClient, func(c *Config) {
		c.Agent.Tools = append(c.Agent.Tools, shell.Tool(shell.Config{Executor: gates}))
		rec.install(c)
		c.TaskWake = func(context.Context, tool.Task) ([]RunOption, bool) {
			return []RunOption{WithModel(wakeClient)}, true
		}
	})
	if _, err := runSync(r, bg, "s", Text("跑两个命令")); err != nil {
		t.Fatal(err)
	}

	close(gates["a"])
	if f := rec.wait(t); f.delivery != ResultStartedRun {
		t.Fatalf("the first result should wake the session: %+v", f)
	}
	<-wakeClient.started
	close(gates["b"])
	waitFor(t, "b's result to be queued in the woken run", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		run := r.active["s"]
		return run != nil && len(run.notices) == 1
	})
	close(wakeClient.release)
	if f := rec.wait(t); f.delivery != ResultInjectedIntoRun {
		t.Fatalf("a result arriving during the woken run should be injected into it, not wake again: %+v", f)
	}
	waitIdle(t, r, "s")

	if got := len(wakeClient.Requests()); got != 2 {
		t.Fatalf("one woken run should handle both results in two turns, got %d calls", got)
	}
	var notices int
	for _, msg := range saved(t, store, "s") {
		if msg.Kind == session.KindTaskResult {
			notices++
		}
	}
	if notices != 2 {
		t.Fatalf("both results should be in history once: %d", notices)
	}
}

// blockingStore holds the writing of task results until release is closed.
type blockingStore struct {
	*session.MemoryStore
	writing chan struct{}
	release chan struct{}
}

func (s *blockingStore) AppendMessages(ctx context.Context, sessionID string, messages ...session.Message) error {
	for _, msg := range messages {
		if msg.Kind == session.KindTaskResult {
			close(s.writing)
			<-s.release
		}
	}
	return s.MemoryStore.AppendMessages(ctx, sessionID, messages...)
}

func TestStartWaitsForATaskResultBeingWritten(t *testing.T) {
	gates := commandGates{"build": make(chan struct{})}
	client := newClient(chattest.CallTools(backgroundCall("c1", "build")), chattest.Say("已开始"), chattest.Say("收到"))
	store := &blockingStore{MemoryStore: session.NewMemoryStore(), writing: make(chan struct{}), release: make(chan struct{})}
	rec := newTaskRecorder()
	r, _ := newRunner(t, client, func(c *Config) {
		c.Store = store
		c.Agent.Tools = append(c.Agent.Tools, shell.Tool(shell.Config{Executor: gates}))
		rec.install(c)
	})
	if _, err := runSync(r, bg, "s", Text("构建一下")); err != nil {
		t.Fatal(err)
	}

	close(gates["build"])
	<-store.writing
	started := make(chan error, 1)
	go func() {
		_, err := runSync(r, bg, "s", Text("好了吗"))
		started <- err
	}()
	select {
	case err := <-started:
		t.Fatalf("Start should wait while a task result is written, got %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if f := rec.wait(t); f.delivery != ResultSavedToHistory {
		t.Fatalf("without TaskWake the result should only be written: %+v", f)
	}
}

func TestDeliverTaskResult(t *testing.T) {
	r, store := newRunner(t, newClient(chattest.Say("你好")))
	if _, err := runSync(r, bg, "s", Text("hi")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []tool.Task{
		{SessionID: "s", Status: tool.TaskFailed},
		{ID: "task_1", SessionID: "s"},
		{ID: "task_1", SessionID: "s", Status: tool.TaskRunning},
	} {
		if _, err := r.DeliverTaskResult(bg, bad); err == nil {
			t.Fatalf("a task without an ID, a session or a final status should be refused: %+v", bad)
		}
	}

	interrupted := tool.Task{ID: "task_1", Type: shell.TaskType, SessionID: "s", Description: "构建", Status: tool.TaskFailed,
		Error: "interrupted by a restart"}
	delivery, err := r.DeliverTaskResult(bg, interrupted)
	if err != nil || delivery != ResultSavedToHistory {
		t.Fatalf("an idle session without TaskWake should get the result in history: %v %v", delivery, err)
	}
	messages := saved(t, store, "s")
	if last := messages[len(messages)-1]; last.Kind != session.KindTaskResult ||
		!strings.Contains(last.Text(), `status="failed"`) || !strings.Contains(last.Text(), "interrupted by a restart") {
		t.Fatalf("the session should receive the interrupted result: %q", last.Text())
	}
}

func TestDeliverTaskResultWakes(t *testing.T) {
	client := newClient(chattest.Say("你好"), chattest.Say("构建被中断了，我重新开始"))
	r, _ := newRunner(t, client, func(c *Config) {
		c.TaskWake = func(context.Context, tool.Task) ([]RunOption, bool) { return nil, true }
	})
	if _, err := runSync(r, bg, "s", Text("hi")); err != nil {
		t.Fatal(err)
	}
	delivery, err := r.DeliverTaskResult(bg, tool.Task{ID: "task_1", SessionID: "s", Status: tool.TaskFailed, Error: "interrupted"})
	if err != nil || delivery != ResultStartedRun {
		t.Fatalf("with TaskWake a delivered result should start a run: %v %v", delivery, err)
	}
	waitIdle(t, r, "s")
	if last := lastRequestMessage(t, client); !strings.Contains(last.TextContent(), "interrupted") {
		t.Fatalf("the woken run should see the delivered result: %+v", last)
	}
}

// breakableStore fails to append once broken is set, as a store that lost its database would.
type breakableStore struct {
	*session.MemoryStore
	broken atomic.Bool
}

func (s *breakableStore) AppendMessages(ctx context.Context, sessionID string, messages ...session.Message) error {
	if s.broken.Load() {
		return errors.New("store is down")
	}
	return s.MemoryStore.AppendMessages(ctx, sessionID, messages...)
}

func TestTaskWakeReportsAResultItCouldNotStore(t *testing.T) {
	gates := commandGates{"build": make(chan struct{})}
	store := &breakableStore{MemoryStore: session.NewMemoryStore()}
	rec := newTaskRecorder()
	r, _ := newRunner(t, newClient(chattest.CallTools(backgroundCall("c1", "build")), chattest.Say("构建已经开始")), func(c *Config) {
		c.Store = store
		c.Agent.Tools = append(c.Agent.Tools, shell.Tool(shell.Config{Executor: gates}))
		rec.install(c)
		c.TaskWake = func(context.Context, tool.Task) ([]RunOption, bool) { return nil, true }
	})
	if _, err := runSync(r, bg, "s", Text("构建一下")); err != nil {
		t.Fatal(err)
	}

	store.broken.Store(true)
	close(gates["build"])
	if f := rec.wait(t); !strings.Contains(f.task.Error, "store is down") {
		t.Fatalf("a result the woken run could not store should be reported on the task: %+v", f)
	}
	waitIdle(t, r, "s")
	_, err := r.DeliverTaskResult(bg, tool.Task{ID: "task_1", SessionID: "s", Status: tool.TaskFailed, Error: "interrupted"})
	if err == nil || !strings.Contains(err.Error(), "store is down") {
		t.Fatalf("delivering a result the woken run could not store should fail: %v", err)
	}
}
