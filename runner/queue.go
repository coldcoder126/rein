package runner

import (
	"context"

	"github.com/coldcoder126/rein/session"
)

// Steer interjects mid-run: the message is injected after the current turn's tools finish and before the next model call,
// to correct or add to the task in progress. It never interrupts tools that are executing. It returns ErrNotRunning when the session is not running.
func (r *Runner) Steer(sessionID string, in Input) error {
	return r.enqueue(sessionID, in, func(h *Run) *[]session.Message { return &h.steer })
}

// FollowUp queues a follow-up message, injected when the model gives its final reply and the run would end; the current task is not affected.
// It returns ErrNotRunning when the session is not running.
func (r *Runner) FollowUp(sessionID string, in Input) error {
	return r.enqueue(sessionID, in, func(h *Run) *[]session.Message { return &h.followUp })
}

// Abort cancels the session's current run and returns the queued messages not yet injected (they do not enter history). It returns an empty queue when idle.
// It does not wait for the run to end; call Wait for that.
func (r *Runner) Abort(sessionID string) Queue {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.active[sessionID]
	if h == nil {
		return Queue{}
	}
	h.queueChanged = true
	h.cancel()
	return h.drainQueues()
}

// Wait waits for the session's current run to end. It returns immediately when idle.
func (r *Runner) Wait(ctx context.Context, sessionID string) error {
	r.mu.Lock()
	h := r.active[sessionID]
	r.mu.Unlock()
	if h == nil {
		return nil
	}
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) enqueue(sessionID string, in Input, queue func(*Run) *[]session.Message) error {
	content, err := in.blocks()
	if err != nil {
		return err
	}
	msg := session.UserMessage(content, nil)
	msg.SessionID = sessionID

	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.active[sessionID]
	// Messages queued after the run started finishing are no longer injected and are treated as if no run were in progress.
	if h == nil || h.ending {
		return ErrNotRunning
	}
	q := queue(h)
	*q = append(*q, msg)
	h.queueChanged = true
	return nil
}

// drainQueues takes every message from both queues; the caller must hold Runner.mu.
func (h *Run) drainQueues() Queue {
	q := Queue{Steer: h.steer, FollowUp: h.followUp}
	h.steer, h.followUp = nil, nil
	return q
}
