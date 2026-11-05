package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
	"github.com/coldcoder126/rein/tool"
)

// outputLimit is how many bytes from the end of the output are kept in a task snapshot.
const outputLimit = 32 << 10

// task is a background task. snapshot and stopped are guarded by Runner.mu.
type task struct {
	snapshot tool.Task
	cancel   context.CancelFunc
	// stopped means the task was halted by Stop.
	stopped bool
	// ended is closed by runTask once the final snapshot is settled; the result has not been sent back to the session yet.
	ended chan struct{}
}

type taskRun func(ctx context.Context, report func(tool.TaskUpdate)) (tool.TaskResult, error)

// startTask registers a task and runs it in the background. Tasks outlive the tool call and run that started them: they are not canceled with them and only Stop halts them.
func (r *Runner) startTask(ctx context.Context, snapshot tool.Task, run taskRun) tool.Task {
	base := context.WithoutCancel(ctx)
	runCtx, cancel := context.WithCancel(base)
	snapshot.Status, snapshot.StartedAt = tool.TaskRunning, time.Now()
	t := &task{snapshot: snapshot, cancel: cancel, ended: make(chan struct{})}
	r.mu.Lock()
	r.tasks[snapshot.ID] = t
	r.mu.Unlock()
	go r.runTask(base, runCtx, t, run)
	return snapshot
}

// runTask runs the task and sends its result back to the session that spawned it when it ends. Callbacks receive base, which stays usable after the task is stopped.
func (r *Runner) runTask(base, ctx context.Context, t *task, run taskRun) {
	defer t.cancel()
	if r.onTaskStarted != nil {
		r.onTaskStarted(base, r.snapshotOf(t))
	}
	result, err := run(ctx, func(u tool.TaskUpdate) {
		snapshot := r.updateTask(t, u)
		if r.onTaskProgressed != nil {
			r.onTaskProgressed(base, snapshot, u)
		}
	})
	snapshot := r.endTask(t, result, err)
	close(t.ended)

	delivery, err := r.deliverTaskResult(base, snapshot)
	r.mu.Lock()
	if err != nil {
		t.snapshot.Error = strings.TrimSpace(t.snapshot.Error + "\nfailed to write the result to the session: " + err.Error())
		snapshot = t.snapshot
	}
	delete(r.tasks, snapshot.ID)
	r.mu.Unlock()
	if r.onTaskFinished != nil {
		r.onTaskFinished(base, snapshot, delivery)
	}
}

func (r *Runner) snapshotOf(t *task) tool.Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return t.snapshot
}

// updateTask merges a progress report into the snapshot: only the tail of the output is kept, and Details are replaced as a whole.
func (r *Runner) updateTask(t *task, u tool.TaskUpdate) tool.Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &t.snapshot
	if u.Output != "" {
		s.Output += u.Output
		if over := len(s.Output) - outputLimit; over > 0 {
			for over < len(s.Output) && !utf8.RuneStart(s.Output[over]) {
				over++
			}
			s.Output = s.Output[over:]
		}
	}
	if u.Details != nil {
		s.Details = u.Details
	}
	return *s
}

// endTask settles the final snapshot from the task's return values.
func (r *Runner) endTask(t *task, result tool.TaskResult, err error) tool.Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &t.snapshot
	s.EndedAt = time.Now()
	s.Result = result.Text
	if result.Details != nil {
		s.Details = result.Details
	}
	switch {
	case err == nil:
		s.Status = tool.TaskCompleted
	case t.stopped:
		s.Status = tool.TaskStopped
	default:
		s.Status = tool.TaskFailed
		s.Error = err.Error()
	}
	return *s
}

// ListTasks returns the background tasks spawned by the session whose results have not been delivered, ordered by start time.
func (r *Runner) ListTasks(sessionID string) []tool.Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasksOf(sessionID)
}

// StopTask stops a background task and waits for it to end. The result is still sent back to the session as a notice, with the stopped status.
// It returns tool.ErrTaskNotFound when the task does not exist or has already ended.
// Do not call it synchronously from this task's callbacks: they run on the task's goroutine and would wait on each other.
func (r *Runner) StopTask(ctx context.Context, id string) error {
	_, err := r.stopTask(ctx, "", id)
	return err
}

// stopTask stops a task; when sessionID is set, only tasks spawned by that session can be stopped.
func (r *Runner) stopTask(ctx context.Context, sessionID, id string) (tool.Task, error) {
	r.mu.Lock()
	t := r.tasks[id]
	if t == nil || (sessionID != "" && t.snapshot.SessionID != sessionID) {
		r.mu.Unlock()
		return tool.Task{}, tool.ErrTaskNotFound
	}
	if t.snapshot.Status == tool.TaskRunning {
		t.stopped = true
		t.cancel()
	}
	r.mu.Unlock()
	select {
	case <-t.ended:
	case <-ctx.Done():
		return tool.Task{}, ctx.Err()
	}
	return r.snapshotOf(t), nil
}

// tasksOf returns snapshots of the tasks spawned by the session; the caller must hold Runner.mu.
func (r *Runner) tasksOf(sessionID string) []tool.Task {
	var out []tool.Task
	for _, t := range r.tasks {
		if t.snapshot.SessionID == sessionID {
			out = append(out, t.snapshot)
		}
	}
	slices.SortFunc(out, func(a, b tool.Task) int { return a.StartedAt.Compare(b.StartedAt) })
	return out
}

// sessionTaskManager is the tool.TaskManager the Runner gives a session: it only sees and acts on tasks spawned by that session.
type sessionTaskManager struct {
	r         *Runner
	sessionID string
}

func (s sessionTaskManager) Start(ctx context.Context, spec tool.TaskSpec) (tool.Task, error) {
	if spec.Run == nil {
		return tool.Task{}, errors.New("runner: TaskSpec.Run must not be nil")
	}
	snapshot := tool.Task{ID: newID("task"), Type: spec.Type, SessionID: s.sessionID, Description: spec.Description}
	return s.r.startTask(ctx, snapshot, spec.Run), nil
}

func (s sessionTaskManager) List(context.Context) []tool.Task { return s.r.ListTasks(s.sessionID) }

func (s sessionTaskManager) Get(_ context.Context, id string) (tool.Task, error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	t := s.r.tasks[id]
	if t == nil || t.snapshot.SessionID != s.sessionID {
		return tool.Task{}, tool.ErrTaskNotFound
	}
	return t.snapshot, nil
}

func (s sessionTaskManager) Stop(ctx context.Context, id string) (tool.Task, error) {
	return s.r.stopTask(ctx, s.sessionID, id)
}

// subAgentExecutor is the Runner's implementation of agent.SubAgentExecutor: sub-tasks are registered in the task table, and the task ID is the child session ID.
type subAgentExecutor struct{ r *Runner }

func (e subAgentExecutor) Start(ctx context.Context, in agent.SubAgentInput) (tool.Task, error) {
	r := e.r
	child, err := New(Config{
		Agent:             in.Agent,
		Store:             r.store,
		Converter:         r.converter,
		Compactor:         r.compactor,
		DisableCompaction: r.compactor == nil,
		Middlewares:       r.middlewares,
	})
	if err != nil {
		return tool.Task{}, err
	}
	details := agent.SubAgentDetails{Agent: in.Agent.Name, Task: in.Task}
	snapshot := tool.Task{ID: newID("session"), Type: agent.SubAgentTaskType, SessionID: in.Parent.SessionID,
		Description: in.Description, Details: details}
	return r.startTask(ctx, snapshot, func(ctx context.Context, report func(tool.TaskUpdate)) (tool.TaskResult, error) {
		return runSubAgent(ctx, child, snapshot.ID, in, details, report)
	}), nil
}

// runSubAgent runs a sub-task on its child session. A sub-task is just an ordinary run on a Runner sharing the store,
// so storing, replay and compaction work as in the main session; the child session records its parent. Every event is reported as progress.
func runSubAgent(ctx context.Context, child *Runner, sessionID string, in agent.SubAgentInput, details agent.SubAgentDetails, report func(tool.TaskUpdate)) (tool.TaskResult, error) {
	asChild := func(o *runOptions) { o.parent = &in.Parent }
	run, err := child.Start(ctx, sessionID, Text(in.Task), asChild)
	if err != nil {
		return tool.TaskResult{Details: details}, err
	}
	// The run does not follow ctx on its own: stopping the task aborts it, and the events are still read to the end.
	defer context.AfterFunc(ctx, func() { run.Abort() })()
	ctx = context.WithoutCancel(ctx)
	for event := range run.Events(ctx) {
		if ae := event.AgentEvent; ae != nil {
			switch ae.Type {
			case agent.EventTurnStart:
				details.Turns++
			case agent.EventToolExecutionStart:
				details.LastTool = ae.ToolCall.Name
			case agent.EventMessageEnd:
				if ae.Message != nil && ae.Message.Role == chat.RoleAssistant {
					details.Usage = details.Usage.Add(ae.Message.Usage)
				}
			}
		}
		report(tool.TaskUpdate{Details: details, Event: event})
	}
	result, runErr := run.Wait(ctx)
	// The sub-task's final reply is its result; when it did not complete normally, it is the last output before the interruption.
	var out tool.TaskResult
	if result != nil {
		details.Usage = result.Usage
		if result.Final != nil {
			out.Text = result.Final.Text()
		}
	}
	out.Details = details
	return out, runErr
}

// deliverTaskResult sends a task's result back to the session that spawned it as a notice and returns how it was delivered:
// while the session runs, the notice is queued until the run injects it or stores it while finishing;
// while the session is idle, it is occupied briefly and the notice is written directly to history.
func (r *Runner) deliverTaskResult(ctx context.Context, t tool.Task) (TaskResultDelivery, error) {
	msg := taskNotice(t)
	sessionID := t.SessionID
	for {
		r.mu.Lock()
		h := r.active[sessionID]
		switch {
		case h == nil:
			// While it is occupied new runs get ErrBusy, and other senders wait for it to be released.
			hold := r.occupy(sessionID, func() {}, true)
			r.mu.Unlock()
			ctx := context.WithoutCancel(ctx)
			_, err := r.occupation(ctx, hold, r.agent.Model, func(st *runState) error { return st.save(ctx, msg) })
			return ResultSavedToHistory, err
		case !h.ending:
			ch := make(chan TaskResultDelivery, 1)
			h.notices = append(h.notices, msg)
			if h.delivered == nil {
				h.delivered = make(map[string]chan TaskResultDelivery)
			}
			h.delivered[msg.ID] = ch
			r.mu.Unlock()
			return <-ch, nil
		default:
			// The run is finishing and no longer accepts notices: wait for it to end, then write to history instead.
			r.mu.Unlock()
			<-h.done
		}
	}
}

// notifyDelivered tells waiting tasks that their results have been delivered. The caller must hold Runner.mu.
// When messages is empty, every task still waiting is told.
func (h *Run) notifyDelivered(delivery TaskResultDelivery, messages []session.Message) {
	if messages == nil {
		for id, ch := range h.delivered {
			ch <- delivery
			delete(h.delivered, id)
		}
		return
	}
	for _, msg := range messages {
		if ch := h.delivered[msg.ID]; ch != nil {
			ch <- delivery
			delete(h.delivered, msg.ID)
		}
	}
}

// taskNotice turns a task's result into the notice sent back to the session: visible to the model, hidden from the user by default, with the task's final snapshot as Details.
func taskNotice(t tool.Task) session.Message {
	var body strings.Builder
	fmt.Fprintf(&body, "<task_result id=\"%s\" type=\"%s\" status=\"%s\" description=\"%s\">\n",
		xmlEscape(t.ID), xmlEscape(t.Type), t.Status, xmlEscape(t.Description))
	if t.Error != "" {
		fmt.Fprintf(&body, "error: %s\n\n", t.Error)
	}
	if t.Result != "" {
		body.WriteString(t.Result + "\n")
	}
	body.WriteString("</task_result>")
	msg := session.UserMessage([]chat.ContentBlock{chat.Text(body.String())}, t)
	msg.ID = newID("msg")
	msg.SessionID = t.SessionID
	msg.Visible, msg.Source, msg.Kind = false, session.SourceSystem, session.KindTaskResult
	return msg
}
