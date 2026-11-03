// Package runner is the Agent runtime: it loads history by session ID, builds the context of each run, drives the Agent,
// persists messages, manages queued messages and history compaction, and hands the events of a run to its caller.
//
// A Runner holds one Agent and one store and serves any number of sessions: different sessions can run concurrently,
// and each session has at most one run at a time. That guarantee only holds within one Runner instance: when several processes or
// instances share a store, the caller must ensure mutual exclusion per session (for example by routing a session to a fixed instance, or with a distributed lock). Who the Agent is (instructions, model, tools) is defined on agent.Agent,
// what is stored is defined in the session package, and the Runner only runs them.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/session"
	"github.com/coldcoder126/rein/tool"
)

var (
	// ErrBusy means the session already has a run in progress.
	ErrBusy = errors.New("runner: session is busy")
	// ErrNotRunning means the session has no run in progress, so messages cannot be queued.
	ErrNotRunning = errors.New("runner: session is not running")
	// ErrInvalidConfig means the Runner configuration is incomplete.
	ErrInvalidConfig = errors.New("runner: invalid config")
)

// Config is the configuration for creating a Runner.
type Config struct {
	// Agent is the Agent to run.
	// Do not modify it after handing it to the Runner: every run of every session reads this one definition without synchronization.
	Agent *agent.Agent
	// Store is the session store; an in-memory store is used when nil.
	Store session.Store
	// Converter converts session messages into model-visible messages; DefaultConverter is used when nil.
	Converter MessageConverter
	// Compactor is the history compaction strategy; SummaryCompactor with defaults is used when nil.
	// There is no compaction when the model declares no context limit (ModelInfo.MaxInputTokens is 0).
	Compactor Compactor
	// DisableCompaction turns compaction off.
	DisableCompaction bool
	// Middlewares are the runtime middlewares, applied to every run on this Runner, including the sub-agents it spawns.
	// Steps every Agent must go through rather than ones belonging to a particular Agent, such as approval, auditing and tracing, go here.
	// They wrap the Agent's own middlewares, so an Agent can only add its own behavior inside them; history compaction is innermost.
	Middlewares []agent.Middleware

	// The callbacks below track background tasks spawned by the Agent (sub-agents, background commands and so on). They are called synchronously
	// on the task's goroutine and must not block for long. task.Type tells the kind of task.

	// OnTaskStarted is called after a task starts running, exactly once per task and before the other callbacks.
	OnTaskStarted func(ctx context.Context, task tool.Task)
	// OnTaskProgressed is called every time a task reports progress, for live display. task is the snapshot with the progress merged;
	// for a sub-agent, update.Event is the Event of the run on the child session, the same as Stream hands to its caller,
	// and for a background command update.Output is the new output.
	OnTaskProgressed func(ctx context.Context, task tool.Task, update tool.TaskUpdate)
	// OnTaskFinished is called after a task ends and its result has been sent back to the session that spawned it, exactly once per task and last.
	// When delivery is ResultSavedToHistory the main Agent has not seen the result yet, and the business layer decides whether to wake it,
	// for example with r.Stream(ctx, task.SessionID, Input{}).
	OnTaskFinished func(ctx context.Context, task tool.Task, delivery TaskResultDelivery)
}

// TaskResultDelivery says how a background task's result was sent back to the session.
type TaskResultDelivery string

const (
	// ResultInjectedIntoRun means the session was running and the result was injected into that run, where the main Agent handles it.
	ResultInjectedIntoRun TaskResultDelivery = "injected_into_run"
	// ResultSavedToHistory means the session was idle and the result was only written to history; the main Agent has not seen it yet.
	ResultSavedToHistory TaskResultDelivery = "saved_to_history"
)

// Runner runs one Agent and serves many sessions. It is safe for concurrent use.
type Runner struct {
	agent     *agent.Agent
	store     session.Store
	converter MessageConverter
	compactor Compactor
	// middlewares are the runtime middlewares; the Runner for sub-agents uses the same set.
	middlewares []agent.Middleware

	onTaskStarted    func(context.Context, tool.Task)
	onTaskProgressed func(context.Context, tool.Task, tool.TaskUpdate)
	onTaskFinished   func(context.Context, tool.Task, TaskResultDelivery)

	mu     sync.Mutex
	active map[string]*runHandle
	// tasks are the background tasks spawned by sessions on this Runner whose results have not been delivered, keyed by task ID.
	tasks map[string]*task
}

// runHandle is the handle for operating a run from outside: Steer, FollowUp, Abort and Wait use it from other goroutines
// to add messages, cancel, or wait for the end. It is registered in Runner.active, and all its fields are guarded by Runner.mu.
// The run's own state is in runState.
type runHandle struct {
	runID  string
	cancel context.CancelFunc
	done   chan struct{}

	steer    []session.Message
	followUp []session.Message
	// queueChanged means the queues were changed by calls outside the run (Steer, FollowUp, Abort);
	// the run emits a queue_update before its next event.
	queueChanged bool

	// notices are task result notices waiting to be injected. They are queued separately from steer: steer messages go back to the caller
	// as Pending when the run ends, while notices must enter history, so those not injected are stored by the run when it ends.
	notices []session.Message
	// delivered registers, by notice message ID, the tasks waiting for their result to be delivered; they receive the delivery method once the notice is injected or stored.
	delivered map[string]chan TaskResultDelivery
	// ending means the run is finishing and no longer accepts notices; senders wait for it to end and write to history instead.
	ending bool
}

// New creates a Runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Agent == nil || cfg.Agent.Model == nil {
		return nil, fmt.Errorf("%w: Agent and its Model must not be nil", ErrInvalidConfig)
	}
	if cfg.Store == nil {
		cfg.Store = session.NewMemoryStore()
	}
	if cfg.Converter == nil {
		cfg.Converter = DefaultConverter{}
	}
	if cfg.DisableCompaction {
		cfg.Compactor = nil
	} else if cfg.Compactor == nil {
		cfg.Compactor = SummaryCompactor{}
	}
	return &Runner{
		agent:     cfg.Agent,
		store:     cfg.Store,
		converter: cfg.Converter,
		compactor: cfg.Compactor,

		middlewares: cfg.Middlewares,

		onTaskStarted:    cfg.OnTaskStarted,
		onTaskProgressed: cfg.OnTaskProgressed,
		onTaskFinished:   cfg.OnTaskFinished,

		active: make(map[string]*runHandle),
		tasks:  make(map[string]*task),
	}, nil
}

// Store returns the underlying session store, for reading history.
func (r *Runner) Store() session.Store { return r.store }

// Running reports whether the session currently has a run in progress.
func (r *Runner) Running(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[sessionID] != nil
}

// acquire occupies the session and registers a run handle, returning ErrBusy when the session already has a run.
// When ending is true the handle accepts no interjections or notices from the start, for occupations that do not run the Agent (such as manual compaction).
func (r *Runner) acquire(sessionID string, cancel context.CancelFunc, ending bool) (*runHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[sessionID] != nil {
		return nil, ErrBusy
	}
	h := &runHandle{runID: newID("run"), cancel: cancel, done: make(chan struct{}), ending: ending}
	r.active[sessionID] = h
	return h, nil
}

// release frees the session: it unregisters the handle, cancels the run and wakes waiters.
func (r *Runner) release(sessionID string, h *runHandle) {
	r.mu.Lock()
	delete(r.active, sessionID)
	r.mu.Unlock()
	h.cancel()
	close(h.done)
}

func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(b[:]))
	}
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}
