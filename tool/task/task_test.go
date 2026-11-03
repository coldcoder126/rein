package task_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/task"
)

type fakeTaskManager struct {
	tasks   map[string]tool.Task
	stopped []string
}

func (f *fakeTaskManager) Start(context.Context, tool.TaskSpec) (tool.Task, error) {
	return tool.Task{}, nil
}

func (f *fakeTaskManager) List(context.Context) []tool.Task {
	var out []tool.Task
	for _, id := range []string{"a", "b"} {
		if t, ok := f.tasks[id]; ok {
			out = append(out, t)
		}
	}
	return out
}

func (f *fakeTaskManager) Get(_ context.Context, id string) (tool.Task, error) {
	t, ok := f.tasks[id]
	if !ok {
		return tool.Task{}, tool.ErrTaskNotFound
	}
	return t, nil
}

func (f *fakeTaskManager) Stop(ctx context.Context, id string) (tool.Task, error) {
	t, err := f.Get(ctx, id)
	if err != nil {
		return t, err
	}
	f.stopped = append(f.stopped, id)
	t.Status = tool.TaskStopped
	return t, nil
}

func execute(t *testing.T, tasks tool.TaskManager, args string) (*tool.Result, error) {
	t.Helper()
	ctx := context.Background()
	if tasks != nil {
		ctx = tool.WithTaskManager(ctx, tasks)
	}
	return task.Tool().Execute(tool.NewContext(ctx, tool.RunInfo{}, chat.ToolCall{Name: task.ToolName}, nil), json.RawMessage(args))
}

func TestStatusAndStop(t *testing.T) {
	tasks := &fakeTaskManager{tasks: map[string]tool.Task{
		"a": {ID: "a", Type: "shell", Description: "npm test", Status: tool.TaskRunning, Output: "PASS"},
		"b": {ID: "b", Type: "sub_agent", Description: "查订单", Status: tool.TaskRunning},
	}}
	result, err := execute(t, tasks, `{"action":"status"}`)
	if err != nil || strings.Contains(result.Content[0].Text, "PASS") || !strings.Contains(result.Content[0].Text, `"id":"b"`) {
		t.Fatalf("the list should contain all tasks without output: %+v %v", result, err)
	}
	result, err = execute(t, tasks, `{"action":"status","id":"a"}`)
	if err != nil || !strings.Contains(result.Content[0].Text, `"output":"PASS"`) {
		t.Fatalf("viewing a single task should include its output: %+v %v", result, err)
	}
	result, err = execute(t, tasks, `{"action":"stop","id":"b"}`)
	if err != nil || len(tasks.stopped) != 1 || result.Details.(tool.Task).Status != tool.TaskStopped {
		t.Fatalf("should stop the task and return its final snapshot: %+v %v", result, err)
	}
}

func TestErrors(t *testing.T) {
	tasks := &fakeTaskManager{tasks: map[string]tool.Task{"a": {ID: "a", Type: "shell", Description: "npm test", Status: tool.TaskRunning}}}
	if _, err := execute(t, tasks, `{"action":"stop","id":"x"}`); err == nil || !strings.Contains(err.Error(), "running tasks: a (shell: npm test)") {
		t.Fatalf("a missing task should list the running tasks: %v", err)
	}
	if _, err := execute(t, tasks, `{"action":"stop"}`); err == nil {
		t.Fatal("stop without an id should fail")
	}
	if result, err := execute(t, &fakeTaskManager{}, `{"action":"status"}`); err != nil || result.Content[0].Text != "No background tasks are running." {
		t.Fatalf("no tasks should give a short note: %+v %v", result, err)
	}
	if _, err := execute(t, nil, `{"action":"status"}`); err == nil {
		t.Fatal("should fail without background task support")
	}
	if nt, ok := task.Tool().(tool.NeedsTaskManager); !ok || !nt.NeedsTaskManager() {
		t.Fatal("the task tool should declare that it needs background tasks")
	}
}
