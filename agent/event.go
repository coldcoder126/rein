package agent

import (
	"context"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

// EventType is the type of an event during an Agent run.
type EventType string

const (
	EventAgentStart          EventType = "agent_start"
	EventAgentEnd            EventType = "agent_end"
	EventTurnStart           EventType = "turn_start"
	EventTurnEnd             EventType = "turn_end"
	EventMessageStart        EventType = "message_start"
	EventMessageUpdate       EventType = "message_update"
	EventMessageEnd          EventType = "message_end"
	EventToolExecutionStart  EventType = "tool_execution_start"
	EventToolExecutionUpdate EventType = "tool_execution_update"
	EventToolExecutionEnd    EventType = "tool_execution_end"
)

// Turn holds a turn's model response and its tool results. Messages injected at the start of a turn (steering, follow-up) are not here;
// they are emitted as message events with the same TurnIndex.
type Turn struct {
	Message     chat.Message
	ToolResults []chat.Message
}

// Event is a structured event emitted by the agent.
type Event struct {
	Type      EventType
	TurnIndex int
	// Message is a snapshot of the assistant or tool result message. It is safe to keep but read-only:
	// it shares the bytes of media data and tool arguments with the streaming message, so do not modify its content.
	Message *chat.Message
	// AssistantEvent is the model stream event carried by message_update; its Message matches the snapshot above.
	AssistantEvent *chat.Event
	ToolCall       *chat.ToolCall
	Update         *tool.Update
	Result         *tool.Result
	Turn           *Turn
	RunResult      *Result
	Err            error
}

// TextDelta returns the reply text delta of this event, or "" when it is not a text delta event.
func (e Event) TextDelta() string { return e.delta(chat.EventTextDelta) }

// ThinkingDelta returns the thinking text delta of this event, or "" when it is not a thinking delta event.
func (e Event) ThinkingDelta() string { return e.delta(chat.EventThinkingDelta) }

func (e Event) delta(kind chat.EventType) string {
	if e.AssistantEvent == nil || e.AssistantEvent.Type != kind {
		return ""
	}
	return e.AssistantEvent.Delta
}

// EventSink receives agent events. Returning an error stops the current run.
type EventSink func(ctx context.Context, event Event) error
