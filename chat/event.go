package chat

// EventType is the type of a streaming chat event.
type EventType string

const (
	EventStart         EventType = "start"
	EventTextStart     EventType = "text_start"
	EventTextDelta     EventType = "text_delta"
	EventTextEnd       EventType = "text_end"
	EventThinkingStart EventType = "thinking_start"
	EventThinkingDelta EventType = "thinking_delta"
	EventThinkingEnd   EventType = "thinking_end"
	// At EventToolCallStart the tool name is known and the arguments are empty; EventToolCallEnd carries the complete call.
	// Tool arguments are not streamed incrementally yet, so there is no tool_call_delta.
	EventToolCallStart EventType = "tool_call_start"
	EventToolCallEnd   EventType = "tool_call_end"
	// EventImage gives a complete model-generated image. Image blocks have no deltas; the event itself closes the block.
	EventImage EventType = "image"
	// EventServerTool gives a complete record of a tool the provider ran on its side, such as a tool search; like an image,
	// the event itself closes the block. It is for progress display only: the record needs no handling.
	EventServerTool EventType = "server_tool"
	EventDone       EventType = "done"
	EventError      EventType = "error"
)

// Event is a streaming model output event. It only describes model output, not tool execution.
type Event struct {
	Type EventType
	// Index is the position in Message.Content of the content block the event belongs to.
	Index int
	// Delta is the incremental text of text_delta / thinking_delta.
	Delta string
	// Content is the complete text of the block at text_end / thinking_end.
	Content string
	// ToolCall is set on tool_call_start / tool_call_end events.
	ToolCall *ToolCall
	// Reason gives the stop reason on done / error events.
	Reason StopReason
	// Message is the assistant message accumulated so far; on done / error it is the final message.
	// It is a live object that is only valid during the callback; call Clone to keep it.
	Message *Message
	// Err gives the typed error on error events.
	Err error
}
