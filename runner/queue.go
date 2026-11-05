package runner

import (
	"github.com/coldcoder126/rein/session"
)

// Steer interjects mid-run: the message is injected after the current turn's tools finish and before the next model call,
// to correct or add to the task in progress. It never interrupts tools that are executing.
// It returns ErrNotRunning once the run is finishing, and for occupations that do not run the Agent (compaction, task results).
func (run *Run) Steer(in Input) error {
	return run.enqueue(in, func() *[]session.Message { return &run.steer })
}

// FollowUp queues a follow-up message, injected when the model gives its final reply and the run would end; the current task is not affected.
// It returns ErrNotRunning in the same cases as Steer.
func (run *Run) FollowUp(in Input) error {
	return run.enqueue(in, func() *[]session.Message { return &run.followUp })
}

// Abort cancels the run and returns the queued messages not yet injected (they do not enter history); it returns an empty queue
// once the run has ended. It does not wait for the run to end; call Wait for that.
func (run *Run) Abort() Queue {
	run.r.mu.Lock()
	defer run.r.mu.Unlock()
	if run.finished {
		return Queue{}
	}
	run.queueChanged = true
	run.cancel()
	return run.drainQueues()
}

func (run *Run) enqueue(in Input, queue func() *[]session.Message) error {
	content, err := in.blocks()
	if err != nil {
		return err
	}
	msg := session.UserMessage(content, nil)
	msg.SessionID = run.sessionID

	run.r.mu.Lock()
	defer run.r.mu.Unlock()
	// Messages queued after the run started finishing are no longer injected.
	if run.ending {
		return ErrNotRunning
	}
	q := queue()
	*q = append(*q, msg)
	run.queueChanged = true
	return nil
}

// drainQueues takes every message from both queues; the caller must hold Runner.mu.
func (run *Run) drainQueues() Queue {
	q := Queue{Steer: run.steer, FollowUp: run.followUp}
	run.steer, run.followUp = nil, nil
	return q
}
