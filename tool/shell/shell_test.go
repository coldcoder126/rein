package shell_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/shell"
)

func TestLocalRun(t *testing.T) {
	var out strings.Builder
	dir := t.TempDir()
	code, err := shell.Local{Dir: dir, Env: []string{"GREETING=hi"}}.Run(context.Background(), `echo $GREETING; pwd; echo oops >&2; exit 3`, &out)
	if err != nil || code != 3 {
		t.Fatalf("should return the command's exit code: %d %v", code, err)
	}
	if got := out.String(); !strings.Contains(got, "hi\n") || !strings.Contains(got, dir) || !strings.Contains(got, "oops") {
		t.Fatalf("stdout and stderr should be merged, using the configured directory and environment: %q", got)
	}
}

type fakeTaskManager struct {
	spec tool.TaskSpec
}

func (f *fakeTaskManager) Start(_ context.Context, spec tool.TaskSpec) (tool.Task, error) {
	f.spec = spec
	return tool.Task{ID: "task-1", Type: spec.Type, Description: spec.Description, Status: tool.TaskRunning}, nil
}
func (f *fakeTaskManager) List(context.Context) []tool.Task { return nil }
func (f *fakeTaskManager) Get(context.Context, string) (tool.Task, error) {
	return tool.Task{}, tool.ErrTaskNotFound
}
func (f *fakeTaskManager) Stop(context.Context, string) (tool.Task, error) {
	return tool.Task{}, tool.ErrTaskNotFound
}

func execute(t *testing.T, ctx context.Context, sh tool.Tool, args string) (*tool.Result, []string) {
	t.Helper()
	var updates []string
	tc := tool.NewContext(ctx, tool.RunInfo{}, chat.ToolCall{Name: shell.ToolName}, func(u tool.Update) error {
		updates = append(updates, u.Content[0].Text)
		return nil
	})
	result, err := sh.Execute(tc, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return result, updates
}

func TestForegroundCommand(t *testing.T) {
	sh := shell.Tool(shell.Config{Executor: shell.Local{}})
	result, updates := execute(t, context.Background(), sh, `{"command":"echo hello"}`)
	if result.IsError || result.Content[0].Text != "hello\n[exit code: 0]" || len(updates) == 0 || updates[0] != "hello\n" {
		t.Fatalf("should return the output and exit code and report output live: %+v %q", result, updates)
	}
	if d := result.Details.(shell.Details); d.Command != "echo hello" || d.ExitCode != 0 {
		t.Fatalf("Details should record the command and exit code: %+v", d)
	}

	result, _ = execute(t, context.Background(), sh, `{"command":"exit 2"}`)
	if !result.IsError || result.Content[0].Text != "(no output)\n[exit code: 2]" {
		t.Fatalf("a non-zero exit code should be an error result: %+v", result)
	}
}

func TestForegroundTimeoutAndLongOutput(t *testing.T) {
	sh := shell.Tool(shell.Config{Executor: shell.Local{}, Timeout: 200 * time.Millisecond})
	start := time.Now()
	result, _ := execute(t, context.Background(), sh, `{"command":"echo started; sleep 10"}`)
	if time.Since(start) > 5*time.Second || !result.IsError || !result.Details.(shell.Details).TimedOut ||
		!strings.Contains(result.Content[0].Text, "started\n[timed out after 200ms") {
		t.Fatalf("a timeout should end the command and keep the output so far: %+v", result)
	}

	result, _ = execute(t, context.Background(), sh, `{"command":"head -c 100000 /dev/zero | tr '\\0' a; echo; echo END"}`)
	text := result.Content[0].Text
	if len(text) > 40<<10 || !strings.Contains(text, "bytes omitted") || !strings.HasSuffix(text, "END\n[exit code: 0]") {
		t.Fatalf("long output should keep only the start and end: len=%d tail=%q", len(text), text[max(0, len(text)-60):])
	}
}

func TestBackgroundCommand(t *testing.T) {
	sh := shell.Tool(shell.Config{Executor: shell.Local{}})
	tc := tool.NewContext(context.Background(), tool.RunInfo{}, chat.ToolCall{}, nil)
	if _, err := sh.Execute(tc, json.RawMessage(`{"command":"echo hi","run_in_background":true}`)); err == nil {
		t.Fatal("background runs should be rejected without background task support")
	}

	tasks := &fakeTaskManager{}
	result, _ := execute(t, tool.WithTaskManager(context.Background(), tasks), sh, `{"command":"echo hi; exit 1","description":"打招呼","run_in_background":true}`)
	if !strings.Contains(result.Content[0].Text, "task id: task-1") || tasks.spec.Type != shell.TaskType || tasks.spec.Description != "打招呼" {
		t.Fatalf("should start a background task and return its ID immediately: %+v %+v", result, tasks.spec)
	}
	var reported []string
	res, err := tasks.spec.Run(context.Background(), func(u tool.TaskUpdate) { reported = append(reported, u.Output) })
	if err == nil || err.Error() != "exit code 1" || res.Text != "hi\n[exit code: 1]" || strings.Join(reported, "") != "hi\n" {
		t.Fatalf("a background command should report output and fail on a non-zero exit code: %+v %v %q", res, err, reported)
	}
}
