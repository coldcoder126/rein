package runner

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"time"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
	"github.com/coldcoder126/rein/tool"
)

// Result is the result of a run. It is still valid when Run.Wait returns an error and contains the messages produced so far.
type Result struct {
	SessionID string
	RunID     string
	// Status is how the run ended; it is StatusFailed when infrastructure such as the store fails.
	Status agent.Status
	// Messages are all messages stored by this run, and Final is the last assistant message.
	Messages []session.Message
	Final    *session.Message
	// Usage includes the tokens consumed by model calls, inside tools and generating summaries.
	Usage chat.Usage
	// Pending are the steer / follow-up messages not yet injected when the run ended; they did not enter history.
	// Messages taken by Abort are returned by Abort, not here.
	Pending Queue
}

// Start processes one user input in a session: it occupies the session, stores the input, loads the history and drives the Agent
// to the end in the background, returning as soon as the run has started. The session is created if it does not exist;
// an empty sessionID generates a new one, see Run.SessionID. It returns ErrBusy at once when the session already has a run in progress,
// and an error when neither WithModel nor Agent.Model gives a model.
//
// The run keeps ctx's values but not its cancellation, so it goes on after the caller returns or a client disconnects;
// stop it with Abort. Watch it with Run.Events or Subscribe and get its result with Run.Wait.
//
// An empty in (Input{}) appends no new input and continues from the existing history, for example to wake the main Agent after a background
// task's result was written to history; the history must then end with a user-side message or a tool result.
func (r *Runner) Start(ctx context.Context, sessionID string, in Input, opts ...RunOption) (*Run, error) {
	var content []chat.ContentBlock
	if len(in.Content) > 0 {
		var err error
		if content, err = in.blocks(); err != nil {
			return nil, err
		}
	}
	var options runOptions
	for _, opt := range opts {
		opt(&options)
	}
	if r.model(options.model) == nil {
		return nil, errNoModel
	}
	if sessionID == "" {
		sessionID = newID("session")
	}
	ctx = context.WithoutCancel(ctx)
	runCtx, cancel := context.WithCancel(ctx)
	run, err := r.acquire(sessionID, cancel, false)
	if err != nil {
		cancel()
		return nil, err
	}
	go func() {
		run.result, run.err = r.process(ctx, runCtx, run, content, options)
		r.release(sessionID, run)
	}()
	return run, nil
}

// ID returns the run ID, which every message stored by the run carries.
func (run *Run) ID() string { return run.id }

// SessionID returns the ID of the session the run belongs to.
func (run *Run) SessionID() string { return run.sessionID }

// Wait waits for the run to end and returns its result. When the model fails, the run is aborted or the turn limit is reached,
// a non-nil error is returned and Result is still valid: Status gives the end status and Messages the messages produced and stored so far.
// ctx only bounds the waiting and does not stop the run. Once Wait returns, the session is free for the next run.
func (run *Run) Wait(ctx context.Context) (*Result, error) {
	select {
	case <-run.done:
		return run.result, run.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Events returns the events of the run from the first one, then each new one as it happens, ending after run_end or when ctx is done.
// Any number of receivers can iterate, each at its own pace, and a slow receiver never holds up the run. Call Wait for the run's error.
//
// The events are kept in memory while the Run is referenced. To bound that, a replayed message_update keeps only its delta
// once a newer message_update exists (AgentEvent.Message is nil); message_end carries the complete message.
func (run *Run) Events(ctx context.Context) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		mu := &run.r.mu
		for next := 0; ; {
			mu.Lock()
			batch := slices.Clone(run.events[next:])
			finished, wake := run.finished, run.wake
			mu.Unlock()
			for _, event := range batch {
				if !yield(event) {
					return
				}
			}
			next += len(batch)
			if len(batch) > 0 {
				continue
			}
			if finished {
				return
			}
			select {
			case <-wake:
			case <-ctx.Done():
				return
			}
		}
	}
}

// Subscribe follows a session: it replays the events of the run in progress, if any, from its start, then hands over the events of
// that run and of every later run of the session in order, until ctx is done. While the session is idle it waits for the next run.
// No run is missed, even one that starts and ends while the receiver is still busy with the previous one.
// It suits views that attach to a session at any time, such as a page reopened in the middle of a reply; earlier runs are read from the store.
func (r *Runner) Subscribe(ctx context.Context, sessionID string) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		r.mu.Lock()
		f := r.followers[sessionID]
		if f == nil {
			f = &follower{latest: r.active[sessionID]}
			if f.latest == nil {
				// A placeholder for the session's last occupation, so that the next run links after it.
				f.latest = &Run{nextReady: make(chan struct{})}
			}
			r.followers[sessionID] = f
		}
		f.n++
		cur := f.latest
		inProgress := r.active[sessionID] == cur
		r.mu.Unlock()
		defer func() {
			r.mu.Lock()
			if f.n--; f.n == 0 {
				delete(r.followers, sessionID)
			}
			r.mu.Unlock()
		}()

		follow := func(run *Run) bool {
			for event := range run.Events(ctx) {
				if !yield(event) {
					return false
				}
			}
			return true
		}
		if inProgress && !follow(cur) {
			return
		}
		for {
			select {
			case <-cur.nextReady:
			case <-ctx.Done():
				return
			}
			if cur = cur.next; !follow(cur) {
				return
			}
		}
	}
}

// follower links the runs of a session while Subscribe calls follow it: latest is the session's most recent occupation, each Run links
// to the one after it, and n counts the followers so that the last one to leave removes it.
type follower struct {
	latest *Run
	n      int
}

// process drives one run on the run's goroutine. ctx carries the caller's values without cancellation and is used for the work
// that must finish anyway; runCtx is canceled by Abort.
func (r *Runner) process(ctx, runCtx context.Context, run *Run, content []chat.ContentBlock, options runOptions) (*Result, error) {
	st := r.newRunState(run, r.model(options.model), options.parent)
	st.emit(Event{Type: EventRunStart})
	err := st.execute(runCtx, content, options)

	r.mu.Lock()
	run.ending = true
	notices := run.notices
	run.notices = nil
	st.result.Pending = run.drainQueues()
	run.queueChanged = run.queueChanged || len(st.result.Pending.Steer)+len(st.result.Pending.FollowUp) > 0
	r.mu.Unlock()
	// Of the messages taken but not injected, steer messages go back to the caller, and task result notices go before those still queued.
	var steer, unseen []session.Message
	for _, msg := range st.injecting() {
		if msg.Kind == session.KindTaskResult {
			unseen = append(unseen, msg)
		} else {
			steer = append(steer, msg)
		}
	}
	st.result.Pending.Steer = append(steer, st.result.Pending.Steer...)
	// Task results that were not injected must not be lost: write them to history so the model sees them in the next run.
	if saveErr := st.save(ctx, append(unseen, notices...)...); saveErr != nil && err == nil {
		err = saveErr
	}
	r.mu.Lock()
	run.notifyDelivered(ResultSavedToHistory, nil)
	r.mu.Unlock()
	if st.result.Status == "" {
		st.result.Status = agent.StatusFailed
	}
	st.emit(Event{Type: EventRunEnd, Result: st.result})
	return st.result, err
}

// occupation drives an occupation that does not run the Agent (manual compaction, writing a task result) on the caller's goroutine:
// it records run_start, does work, ends with run_end and the Result, and frees the session.
func (r *Runner) occupation(ctx context.Context, run *Run, model chat.Client, work func(st *runState) error) (*Result, error) {
	st := r.newRunState(run, model, nil)
	st.emit(Event{Type: EventRunStart})
	err := work(st)
	switch {
	case err == nil:
		st.result.Status = agent.StatusCompleted
	case ctx.Err() != nil:
		st.result.Status = agent.StatusAborted
	default:
		st.result.Status = agent.StatusFailed
	}
	st.emit(Event{Type: EventRunEnd, Result: st.result})
	run.result, run.err = st.result, err
	r.release(run.sessionID, run)
	return st.result, err
}

// newRunState creates the state of a run on the given model. A non-nil parent is the run that started it as a sub-task.
func (r *Runner) newRunState(run *Run, model chat.Client, parent *tool.RunInfo) *runState {
	info := tool.RunInfo{SessionID: run.sessionID, RunID: run.id, RootSessionID: run.sessionID}
	if parent != nil {
		info.ParentSessionID, info.RootSessionID = parent.SessionID, parent.RootSessionID
	}
	return &runState{r: r, run: run, info: info, model: model, result: &Result{SessionID: run.sessionID, RunID: run.id}}
}

// runState is the private state of a run, only accessed on the run's goroutine, so it needs no lock.
// It connects the Agent run to the session: the callbacks given to the Agent (storing, taking queues, compaction, background tasks) hang off it.
// The part shared with others is in run and requires Runner.mu.
type runState struct {
	r    *Runner
	run  *Run
	info tool.RunInfo
	// model is the model of this run: the one given by WithModel, or Agent.Model.
	model  chat.Client
	result *Result
	// batches are messages taken from the queues and waiting for the Agent to inject them. They are stored only when actually injected,
	// so messages that were canceled or never injected because the turns ran out do not enter history.
	batches []injectBatch
	// runContext is the run-scoped context of this run, merged again whenever the history is rebuilt mid-run.
	runContext []session.Message
	// recovered means this run has already recovered from one context overflow; another overflow is not compacted and retried.
	recovered bool
}

type injectBatch struct {
	messages []session.Message
	// remaining is how many model messages this batch converts to (consecutive user messages are merged).
	remaining int
	saved     bool
}

// emit records an event in the run's log for its receivers. When the queues were changed by calls outside the run, a queue_update
// is recorded first, so every event is recorded in order by the run itself.
func (st *runState) emit(event Event) {
	st.r.mu.Lock()
	defer st.r.mu.Unlock()
	if st.run.queueChanged {
		st.run.queueChanged = false
		queue := &Queue{Steer: session.CloneMessages(st.run.steer), FollowUp: session.CloneMessages(st.run.followUp)}
		st.run.record(Event{Type: EventQueueUpdate, SessionID: st.info.SessionID, RunID: st.info.RunID, Queue: queue})
	}
	event.SessionID, event.RunID = st.info.SessionID, st.info.RunID
	st.run.record(event)
}

// record appends an event to the log and wakes the receivers. Only the latest message_update keeps its message snapshot:
// every snapshot holds the whole message so far, and keeping one per delta would grow quadratically with the reply.
// The caller must hold Runner.mu.
func (run *Run) record(event Event) {
	if event.AgentEvent != nil && event.AgentEvent.Type == agent.EventMessageUpdate {
		if i := run.lastUpdate; i >= 0 {
			run.events[i] = withoutSnapshot(run.events[i])
		}
		run.lastUpdate = len(run.events)
	}
	run.events = append(run.events, event)
	close(run.wake)
	run.wake = make(chan struct{})
}

// withoutSnapshot returns a copy of a message_update event without its message snapshot, keeping the delta.
// It copies rather than modifies, since receivers may still hold the original.
func withoutSnapshot(event Event) Event {
	ae := *event.AgentEvent
	ae.Message = nil
	if ae.AssistantEvent != nil {
		ce := *ae.AssistantEvent
		ce.Message = nil
		ae.AssistantEvent = &ce
	}
	event.AgentEvent = &ae
	return event
}

func (st *runState) execute(ctx context.Context, content []chat.ContentBlock, options runOptions) error {
	r := st.r
	if err := r.store.EnsureSession(ctx, session.Session{ID: st.info.SessionID, ParentID: st.info.ParentSessionID, CreatedAt: time.Now()}); err != nil {
		return err
	}
	var initial []session.Message
	for _, input := range options.context {
		msg := input.message()
		if msg.Scope == session.ScopePersistent {
			initial = append(initial, msg)
		} else {
			st.runContext = append(st.runContext, msg)
		}
	}
	if len(content) > 0 {
		initial = append(initial, session.UserMessage(content, options.details))
	}
	if err := st.save(ctx, initial...); err != nil {
		return err
	}
	if _, err := st.compact(ctx, CompactAuto, ""); err != nil {
		return err
	}

	// Middlewares from outside in: the runtime's, the Agent's own, and this run's history compaction.
	// The Agent definition is shared, so the runtime middlewares are attached to a copy.
	a := *r.agent
	a.Middlewares = slices.Concat(r.middlewares, a.Middlewares)
	in := agent.Input{
		RunInfo:     st.info,
		Model:       st.model,
		Emit:        st.handleAgentEvent,
		Middlewares: []agent.Middleware{{WrapTurn: st.wrapTurn}},
		// Task result notices are taken in two places: those arriving between turns are injected in the next turn, and those arriving when the loop would end make it run one more turn.
		Steering: func(ctx context.Context) ([]chat.Message, error) {
			return st.drain(ctx, &st.run.notices, &st.run.steer)
		},
		FollowUp: func(ctx context.Context) ([]chat.Message, error) {
			return st.drain(ctx, &st.run.notices, &st.run.followUp)
		},
	}

	// Background tasks spawned by the main run are executed by the Runner, and each sub-agent task runs in a session of its own.
	// Sub-tasks cannot spawn background tasks: their ctx inherits from the parent run, so the parent session's task manager must be removed explicitly.
	if options.parent == nil {
		ctx = tool.WithTaskManager(ctx, sessionTaskManager{r, st.info.SessionID})
		ctx = agent.WithSubAgentExecutor(ctx, subAgentExecutor{r})
	} else {
		ctx = tool.WithTaskManager(ctx, nil)
	}
	messages, err := st.buildMessages(ctx)
	if err != nil {
		return err
	}
	in.Messages = messages
	result, err := a.Run(ctx, in)
	if result != nil {
		st.result.Usage = st.result.Usage.Add(result.Usage)
		st.result.Status = result.Status
	}
	return err
}

// drain takes all messages from the queues in order, converts them into chat messages and records them as a batch for the Agent to inject.
// The queues point to fields in run and are read and written while holding Runner.mu.
func (st *runState) drain(ctx context.Context, queues ...*[]session.Message) ([]chat.Message, error) {
	st.r.mu.Lock()
	var messages []session.Message
	for _, q := range queues {
		// The notice queue is not part of queue_update, so taking from it does not count as a queue change.
		if q != &st.run.notices && len(*q) > 0 {
			st.run.queueChanged = true
		}
		messages = append(messages, *q...)
		*q = nil
	}
	st.r.mu.Unlock()
	if len(messages) == 0 {
		return nil, nil
	}
	converted, err := st.r.converter.ToChatMessages(ctx, messages)
	if err != nil {
		return nil, err
	}
	if len(converted) == 0 {
		return nil, nil
	}
	for _, msg := range converted {
		if msg.Role != chat.RoleUser {
			return nil, fmt.Errorf("runner: MessageConverter turned a queued message into a %s message; only user messages are allowed", msg.Role)
		}
	}
	st.batches = append(st.batches, injectBatch{messages: messages, remaining: len(converted)})
	return converted, nil
}

// injecting returns the messages taken but not yet injected by the Agent.
func (st *runState) injecting() []session.Message {
	var out []session.Message
	for _, batch := range st.batches {
		if !batch.saved {
			out = append(out, batch.messages...)
		}
	}
	return out
}

func (st *runState) handleAgentEvent(ctx context.Context, event agent.Event) error {
	if event.Type == agent.EventMessageEnd && event.Message != nil {
		switch event.Message.Role {
		case chat.RoleAssistant, chat.RoleTool:
			if err := st.save(ctx, session.FromChat(*event.Message)); err != nil {
				return err
			}
		case chat.RoleUser:
			// The Agent injected a queued message: only now is the corresponding batch of session messages stored, and task results in it count as delivered.
			if len(st.batches) > 0 {
				batch := &st.batches[0]
				if !batch.saved {
					batch.saved = true
					if err := st.save(ctx, batch.messages...); err != nil {
						return err
					}
					st.r.mu.Lock()
					st.run.notifyDelivered(ResultInjectedIntoRun, batch.messages)
					st.r.mu.Unlock()
				}
				if batch.remaining--; batch.remaining <= 0 {
					st.batches = st.batches[1:]
				}
			}
		}
	}
	st.emit(Event{Type: EventAgentEvent, AgentEvent: &event})
	return nil
}

// buildMessages loads the history from the store and converts it into model-visible messages, applying the latest summary and merging this run's context.
func (st *runState) buildMessages(ctx context.Context) ([]chat.Message, error) {
	all, err := st.r.store.ListMessages(ctx, st.info.SessionID)
	if err != nil {
		return nil, err
	}
	messages, _ := effectiveMessages(all)
	messages = mergeRunContext(messages, st.runContext)
	out, err := st.r.converter.ToChatMessages(ctx, messages)
	if err != nil {
		return nil, err
	}
	// A failed or canceled assistant message at the end is not replayed to the model; once it is removed the history ends with a user message or a tool result,
	// which is needed to continue after compacting on overflow.
	for n := len(out); n > 0; n = len(out) {
		last := out[n-1]
		if last.Role != chat.RoleAssistant || (last.StopReason != chat.StopReasonError && last.StopReason != chat.StopReasonAborted) {
			break
		}
		out = out[:n-1]
	}
	return out, nil
}

// mergeRunContext inserts the run-scoped context before the last user message, so it merges with the user input into one model message.
func mergeRunContext(messages, runContext []session.Message) []session.Message {
	if len(runContext) == 0 {
		return messages
	}
	insertAt := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Type == session.TypeUser {
			insertAt = i
			break
		}
	}
	out := make([]session.Message, 0, len(messages)+len(runContext))
	out = append(out, messages[:insertAt]...)
	out = append(out, session.CloneMessages(runContext)...)
	return append(out, messages[insertAt:]...)
}

// save writes messages to the store and adds them to the run result. message_saved is emitted only after they are stored.
func (st *runState) save(ctx context.Context, messages ...session.Message) error {
	if len(messages) == 0 {
		return nil
	}
	// Messages already produced must still be written after the run is canceled (such as a half-generated assistant message), so storing ignores cancellation.
	ctx = context.WithoutCancel(ctx)
	now := time.Now()
	for i := range messages {
		if messages[i].ID == "" {
			messages[i].ID = newID("msg")
		}
		messages[i].SessionID = st.info.SessionID
		messages[i].RunID = st.info.RunID
		if messages[i].Scope == "" {
			messages[i].Scope = session.ScopePersistent
		}
		if messages[i].CreatedAt.IsZero() {
			messages[i].CreatedAt = now
		}
	}
	if err := st.r.store.AppendMessages(ctx, st.info.SessionID, messages...); err != nil {
		return err
	}
	for _, msg := range messages {
		msg := msg.Clone()
		st.result.Messages = append(st.result.Messages, msg)
		if msg.Type == session.TypeAssistant {
			st.result.Final = &msg
		}
		st.emit(Event{Type: EventMessageSaved, Message: &msg})
		if msg.Type == session.TypeSummary {
			st.emit(Event{Type: EventCompaction, Message: &msg})
		}
	}
	return nil
}
