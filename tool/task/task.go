// Package task provides the built-in background task tool task: it lists and stops the background tasks spawned by the current session,
// for every kind of task, including sub-agents and background commands.
//
// Tasks are started by their own tools (such as sub_agent and the background mode of shell); when you provide those, provide this tool too,
// or the model cannot check on or stop its tasks. It depends on the tool.TaskManager provided by the runtime and is hidden without one.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

// ToolName is the name of the background task tool.
const ToolName = "task"

// The tool's two operations, chosen with the action parameter.
const (
	ActionStatus = "status"
	ActionStop   = "stop"
)

type params struct {
	Action string `json:"action" jsonschema:"enum=status,enum=stop,description=The operation to perform."`
	ID     string `json:"id,omitempty" jsonschema:"description=The task id. Required for stop; for status, omit it to list all running tasks."`
}

const description = `Check or stop background tasks you started, such as sub-agents and background shell commands.
Each task's result is sent to you automatically as a <task_result> message when it finishes, so do not poll.

- action=status without id: list the tasks still running.
- action=status with id: show one task in detail, including the latest output.
- action=stop with id: stop a running task.`

// Tool returns the background task tool.
func Tool() tool.Tool {
	return taskTool{tool.New(ToolName, description, run)}
}

type taskTool struct{ tool.Tool }

func (taskTool) NeedsTaskManager() bool { return true }

// summary is one entry of the list, without output or result, to save context.
type summary struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Status      tool.TaskStatus `json:"status"`
	StartedAt   time.Time       `json:"started_at"`
}

func run(ctx *tool.Context, in params) (*tool.Result, error) {
	tasks := tool.TaskManagerFrom(ctx)
	if tasks == nil {
		return nil, errors.New("background tasks are not available here")
	}
	switch in.Action {
	case ActionStatus:
		if in.ID == "" {
			list := tasks.List(ctx)
			if len(list) == 0 {
				return tool.Text("No background tasks are running."), nil
			}
			out := make([]summary, 0, len(list))
			for _, t := range list {
				out = append(out, summary{ID: t.ID, Type: t.Type, Description: t.Description, Status: t.Status, StartedAt: t.StartedAt})
			}
			return jsonResult(out)
		}
		t, err := tasks.Get(ctx, in.ID)
		if err != nil {
			return nil, notFound(ctx, tasks, in.ID, err)
		}
		return jsonResult(t)
	case ActionStop:
		if in.ID == "" {
			return nil, errors.New("stop requires id")
		}
		t, err := tasks.Stop(ctx, in.ID)
		if err != nil {
			return nil, notFound(ctx, tasks, in.ID, err)
		}
		return jsonResult(t)
	}
	return nil, fmt.Errorf("unknown action %q", in.Action)
}

// notFound lists the running tasks when a task cannot be found, so the model can correct itself.
func notFound(ctx *tool.Context, tasks tool.TaskManager, id string, err error) error {
	if !errors.Is(err, tool.ErrTaskNotFound) {
		return err
	}
	list := tasks.List(ctx)
	if len(list) == 0 {
		return fmt.Errorf("task %q not found or already finished; no tasks are running", id)
	}
	running := make([]string, 0, len(list))
	for _, t := range list {
		running = append(running, fmt.Sprintf("%s (%s: %s)", t.ID, t.Type, t.Description))
	}
	return fmt.Errorf("task %q not found or already finished; running tasks: %s", id, strings.Join(running, ", "))
}

func jsonResult(v any) (*tool.Result, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &tool.Result{Content: []chat.ContentBlock{chat.Text(string(data))}, Details: v}, nil
}
