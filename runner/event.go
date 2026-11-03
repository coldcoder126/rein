package runner

import (
	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/session"
)

// EventType is the type of an event during a run.
type EventType string

const (
	EventRunStart EventType = "run_start"
	// EventRunEnd is the last event of a run and carries the run result.
	EventRunEnd EventType = "run_end"
	// EventMessageSaved is emitted after a message has been stored.
	EventMessageSaved EventType = "message_saved"
	// EventQueueUpdate is emitted when the queued messages change.
	EventQueueUpdate EventType = "queue_update"
	// EventAgentEvent forwards events of the Agent run (model stream, tool execution and so on).
	EventAgentEvent EventType = "agent_event"
	// EventCompaction is emitted after the history was compacted; Message is the stored summary message.
	EventCompaction EventType = "compaction"
)

// Queue holds the messages waiting to be injected.
type Queue struct {
	Steer    []session.Message
	FollowUp []session.Message
}

// Event is an event during a run.
type Event struct {
	Type      EventType
	SessionID string
	RunID     string

	Message    *session.Message
	AgentEvent *agent.Event
	Queue      *Queue
	// Result carries the run result on run_end.
	Result *Result
}

// TextDelta returns the reply text delta of this event, or "" when it is not a text delta event.
func (e Event) TextDelta() string {
	if e.AgentEvent == nil {
		return ""
	}
	return e.AgentEvent.TextDelta()
}

// ThinkingDelta returns the thinking text delta of this event, or "" when it is not a thinking delta event.
func (e Event) ThinkingDelta() string {
	if e.AgentEvent == nil {
		return ""
	}
	return e.AgentEvent.ThinkingDelta()
}
