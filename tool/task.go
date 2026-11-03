package tool

import (
	"context"
	"errors"
	"time"
)

// TaskStatus is the status of a background task.
type TaskStatus string

const (
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	// TaskFailed means the task did not complete normally; see Task.Error for why.
	TaskFailed  TaskStatus = "failed"
	TaskStopped TaskStatus = "stopped"
)

// Task is a snapshot of a background task. Sub-agents, background commands and other kinds of tasks share it, told apart by Type.
type Task struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// SessionID is the session that spawned the task; its result is sent back there.
	SessionID   string     `json:"session_id"`
	Description string     `json:"description"`
	Status      TaskStatus `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     time.Time  `json:"ended_at,omitzero"`
	// Output is the tail of what the task has printed; only the latest part is kept while it runs.
	Output string `json:"output,omitempty"`
	// Result is the task's final result, sent back to the session as the notice body.
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	// Details hold type-specific information, such as a sub-agent's turns or a command's exit code.
	Details any `json:"details,omitempty"`
}

// TaskSpec describes a task to run in the background.
type TaskSpec struct {
	Type        string
	Description string
	// Run executes the task until it completes or ctx is canceled, reporting progress through report.
	// Returning an error fails the task; a task halted by Stop has the stopped status.
	Run func(ctx context.Context, report func(TaskUpdate)) (TaskResult, error)
}

// TaskUpdate is one progress report of a running task.
type TaskUpdate struct {
	// Output is new output, appended to Task.Output.
	Output string
	// Details, when set, replace Task.Details.
	Details any
	// Event is the task's own progress event, passed unchanged to the runtime's progress callback, such as a sub-agent's run events.
	Event any
}

// TaskResult is the result of a finished task.
type TaskResult struct {
	Text string
	// Details, when set, replace Task.Details.
	Details any
}

// ErrTaskNotFound means the task does not exist, has ended and delivered its result, or belongs to another session.
var ErrTaskNotFound = errors.New("tool: task not found or already finished")

// TaskManager manages the background tasks of one session and is provided by the runtime (the runner): tasks outlive the tool
// call that started them, and their results come back to the session as notices. It only sees and acts on tasks spawned by this session. Its methods may be called concurrently.
type TaskManager interface {
	// Start starts a task and returns immediately.
	Start(ctx context.Context, spec TaskSpec) (Task, error)
	// List returns the tasks spawned by the session whose results have not been delivered, ordered by start time.
	List(ctx context.Context) []Task
	// Get returns a snapshot of a task.
	Get(ctx context.Context, id string) (Task, error)
	// Stop stops a task and returns its final snapshot once it has ended.
	Stop(ctx context.Context, id string) (Task, error)
}

// NeedsTaskManager is an optional interface for tools: returning true means the tool depends on background tasks, and the Agent hides it when there is no TaskManager.
type NeedsTaskManager interface {
	NeedsTaskManager() bool
}

type taskManagerKey struct{}

// WithTaskManager sets who manages background tasks under this ctx; a nil m means background tasks are not supported.
func WithTaskManager(ctx context.Context, m TaskManager) context.Context {
	return context.WithValue(ctx, taskManagerKey{}, m)
}

// TaskManagerFrom returns the background task manager under ctx, or nil when background tasks are not supported. Tools just call TaskManagerFrom(ctx).
func TaskManagerFrom(ctx context.Context) TaskManager {
	m, _ := ctx.Value(taskManagerKey{}).(TaskManager)
	return m
}
