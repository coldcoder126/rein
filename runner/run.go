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

// Result is the result of a run. It is still valid when Run returns an error and contains the messages produced so far.
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

// Run processes one user input in a session: it stores it, loads the history and drives the Agent to the end.
// The session is created if it does not exist; an empty sessionID generates a new one, see Result.SessionID.
// It returns ErrBusy when the session already has a run in progress.
//
// An empty in (Input{}) appends no new input and continues from the existing history, for example to wake the main Agent after a background
// task's result was written to history; the history must then end with a user-side message or a tool result.
//
// When a model call fails, the run is canceled or the turn limit is reached, a non-nil error is returned and Result is still valid:
// Status gives the end status and Messages the messages produced and stored so far.
func (r *Runner) Run(ctx context.Context, sessionID string, in Input, opts ...RunOption) (*Result, error) {
	return r.run(ctx, sessionID, in, opts, nil)
}

// Stream does the same as Run and hands the events of the run to the caller one by one.
//
// The run follows the caller: it continues only after an event is taken, and stops when the caller stops iterating or ctx is canceled.
// To keep a run going after a client disconnects, or to fan events out to several receivers, iterate and forward in your own goroutine.
//
// error appears at most once, in the last iteration, with the same meaning as Run's return value. Once the run has started,
// the last event is run_end and its Result is the run result; when it could not start (for example ErrBusy) the event is the zero value.
func (r *Runner) Stream(ctx context.Context, sessionID string, in Input, opts ...RunOption) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		events := make(chan Event)
		// stopped is closed when iteration ends, so the run no longer waits for the caller to take events.
		stopped := make(chan struct{})
		defer close(stopped)
		var err error
		go func() {
			defer close(events)
			_, err = r.run(ctx, sessionID, in, opts, func(event Event) {
				select {
				case events <- event:
				case <-stopped:
				}
			})
		}()

		var end Event
		for event := range events {
			if event.Type == EventRunEnd {
				end = event
				continue
			}
			if !yield(event, nil) {
				// The caller stopped receiving: cancel the run and wait for it to end; afterwards the session can run again.
				cancel()
				for range events {
				}
				return
			}
		}
		yield(end, err)
	}
}

func (r *Runner) run(ctx context.Context, sessionID string, in Input, opts []RunOption, sink func(Event)) (*Result, error) {
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
	if sessionID == "" {
		sessionID = newID("session")
	}
	runCtx, cancel := context.WithCancel(ctx)
	h, err := r.acquire(sessionID, cancel, false)
	if err != nil {
		cancel()
		return nil, err
	}
	defer r.release(sessionID, h)

	info := tool.RunInfo{SessionID: sessionID, RunID: h.runID, RootSessionID: sessionID}
	if parent := options.parent; parent != nil {
		info.ParentSessionID, info.RootSessionID = parent.SessionID, parent.RootSessionID
	}
	st := &runState{r: r, handle: h, sink: sink, info: info, result: &Result{SessionID: sessionID, RunID: h.runID}}
	st.emit(Event{Type: EventRunStart})
	err = st.execute(runCtx, content, options)

	r.mu.Lock()
	h.ending = true
	notices := h.notices
	h.notices = nil
	st.result.Pending = h.drainQueues()
	h.queueChanged = h.queueChanged || len(st.result.Pending.Steer)+len(st.result.Pending.FollowUp) > 0
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
	h.notifyDelivered(ResultSavedToHistory, nil)
	r.mu.Unlock()
	if st.result.Status == "" {
		st.result.Status = agent.StatusFailed
	}
	st.emit(Event{Type: EventRunEnd, Result: st.result})
	return st.result, err
}

// runState is the private state of a run, only accessed on the run's goroutine, so it needs no lock.
// It connects the Agent run to the session: the callbacks given to the Agent (storing, taking queues, compaction, background tasks) hang off it.
// The part shared with others is in handle and requires Runner.mu.
type runState struct {
	r      *Runner
	handle *runHandle
	// sink receives the events of this run; when nil no events are produced.
	sink   func(Event)
	info   tool.RunInfo
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

// emit hands an event to the caller. When the queues were changed by calls outside the run, a queue_update is emitted first,
// so every event is sent in order by the run itself.
func (st *runState) emit(event Event) {
	if st.sink == nil {
		return
	}
	st.r.mu.Lock()
	var queue *Queue
	if st.handle.queueChanged {
		st.handle.queueChanged = false
		queue = &Queue{Steer: session.CloneMessages(st.handle.steer), FollowUp: session.CloneMessages(st.handle.followUp)}
	}
	st.r.mu.Unlock()
	if queue != nil {
		st.sink(Event{Type: EventQueueUpdate, SessionID: st.info.SessionID, RunID: st.info.RunID, Queue: queue})
	}
	event.SessionID, event.RunID = st.info.SessionID, st.info.RunID
	st.sink(event)
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
		Emit:        st.handleAgentEvent,
		Middlewares: []agent.Middleware{{WrapTurn: st.wrapTurn}},
		// Task result notices are taken in two places: those arriving between turns are injected in the next turn, and those arriving when the loop would end make it run one more turn.
		Steering: func(ctx context.Context) ([]chat.Message, error) {
			return st.drain(ctx, &st.handle.notices, &st.handle.steer)
		},
		FollowUp: func(ctx context.Context) ([]chat.Message, error) {
			return st.drain(ctx, &st.handle.notices, &st.handle.followUp)
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
// The queues point to fields in handle and are read and written while holding Runner.mu.
func (st *runState) drain(ctx context.Context, queues ...*[]session.Message) ([]chat.Message, error) {
	st.r.mu.Lock()
	var messages []session.Message
	for _, q := range queues {
		// The notice queue is not part of queue_update, so taking from it does not count as a queue change.
		if q != &st.handle.notices && len(*q) > 0 {
			st.handle.queueChanged = true
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
					st.handle.notifyDelivered(ResultInjectedIntoRun, batch.messages)
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
