package chat

import (
	"encoding/json"
	"time"
)

type blockKind int

const (
	blockNone blockKind = iota
	blockText
	blockThinking
	blockToolCall
)

// Builder accumulates a provider's streaming output and produces an event sequence that honors the Client contract.
//
// Every provider emits events through a Builder: it guarantees that start comes first, blocks are closed in pairs, there is exactly one terminal event, and Event.Message is always the message accumulated so far.
// Each method returns false when the caller has stopped iterating; the provider should return immediately.
type Builder struct {
	yield func(Event) bool
	msg   Message

	started  bool
	finished bool
	stopped  bool

	open      blockKind
	openIndex int
}

// NewBuilder creates a Builder. The provider, API and model of info are recorded on the assistant message and used to tell whether a replay goes to the same model.
func NewBuilder(yield func(Event) bool, info ModelInfo) *Builder {
	return &Builder{
		yield: yield,
		msg: Message{
			Role:      RoleAssistant,
			Provider:  info.Provider,
			API:       info.API,
			Model:     info.Model,
			CreatedAt: time.Now(),
		},
	}
}

// Finished reports whether a terminal event has been emitted or the caller has stopped iterating.
func (b *Builder) Finished() bool { return b.finished || b.stopped }

// SetResponse records the response ID and the actual model returned by the server.
func (b *Builder) SetResponse(id, model string) {
	b.msg.ResponseID = id
	b.msg.ResponseModel = model
}

// SetUsage updates the accumulated token usage.
func (b *Builder) SetUsage(u Usage) { b.msg.Usage = u }

func (b *Builder) emit(e Event) bool {
	if b.stopped {
		return false
	}
	e.Message = &b.msg
	if !b.yield(e) {
		b.stopped = true
		return false
	}
	return true
}

// Start emits the start event. Calling it again has no effect.
func (b *Builder) Start() bool {
	if b.started {
		return !b.stopped
	}
	b.started = true
	return b.emit(Event{Type: EventStart})
}

// TextDelta appends a text delta, opening a text block if needed.
func (b *Builder) TextDelta(s string) bool {
	if s == "" {
		return !b.Finished()
	}
	if !b.ensureBlock(blockText) {
		return false
	}
	b.msg.Content[b.openIndex].Text += s
	return b.emit(Event{Type: EventTextDelta, Index: b.openIndex, Delta: s})
}

// ThinkingDelta appends a thinking delta, opening a thinking block if needed.
func (b *Builder) ThinkingDelta(s string) bool {
	if s == "" {
		return !b.Finished()
	}
	if !b.ensureBlock(blockThinking) {
		return false
	}
	b.msg.Content[b.openIndex].Text += s
	return b.emit(Event{Type: EventThinkingDelta, Index: b.openIndex, Delta: s})
}

// ThinkingSignature sets the signature of the current thinking block, opening one if needed.
func (b *Builder) ThinkingSignature(sig string) bool {
	if sig == "" {
		return !b.Finished()
	}
	if !b.ensureBlock(blockThinking) {
		return false
	}
	b.msg.Content[b.openIndex].Signature += sig
	return true
}

// RedactedThinking appends a redacted thinking block; data is the encrypted payload that must be sent back unchanged.
func (b *Builder) RedactedThinking(data string) bool {
	if !b.Start() || !b.EndBlock() {
		return false
	}
	idx := len(b.msg.Content)
	b.msg.Content = append(b.msg.Content, ContentBlock{
		Type: ContentTypeThinking, Redacted: true, Signature: data,
	})
	return b.emit(Event{Type: EventThinkingStart, Index: idx}) &&
		b.emit(Event{Type: EventThinkingEnd, Index: idx})
}

// EndBlock closes the open text or thinking block. It does nothing when no block is open.
func (b *Builder) EndBlock() bool {
	switch b.open {
	case blockText:
		b.open = blockNone
		return b.emit(Event{Type: EventTextEnd, Index: b.openIndex, Content: b.msg.Content[b.openIndex].Text})
	case blockThinking:
		b.open = blockNone
		return b.emit(Event{Type: EventThinkingEnd, Index: b.openIndex, Content: b.msg.Content[b.openIndex].Text})
	}
	return !b.stopped
}

// Image appends a complete model-generated image and emits an image event.
func (b *Builder) Image(mimeType string, data []byte) bool {
	return b.ImageSigned(mimeType, data, "")
}

// ImageSigned appends a generated image with a signature, a provider-private identifier that must be sent back unchanged on replay.
func (b *Builder) ImageSigned(mimeType string, data []byte, signature string) bool {
	if !b.Start() || !b.EndBlock() {
		return false
	}
	idx := len(b.msg.Content)
	b.msg.Content = append(b.msg.Content, ContentBlock{
		Type: ContentTypeMedia, MIMEType: mimeType, Data: data, Signature: signature,
	})
	return b.emit(Event{Type: EventImage, Index: idx})
}

// SignOpenBlock sets the signature of the open text or tool call block. It does nothing when no block is open.
// A signature is a provider-private identifier that must be sent back unchanged on replay and is only valid for the original model.
func (b *Builder) SignOpenBlock(signature string) {
	if b.open == blockText || b.open == blockToolCall {
		b.msg.Content[b.openIndex].Signature = signature
	}
}

// CurrentIndex returns the position of the open block in Message.Content, or -1 when no block is open.
func (b *Builder) CurrentIndex() int {
	if b.open == blockNone {
		return -1
	}
	return b.openIndex
}

// SetSignature sets the signature of an existing block, which may already be closed. Signatures are not
// part of the event content; they appear in the final message (the Message of the done / error event).
func (b *Builder) SetSignature(index int, signature string) {
	if index >= 0 && index < len(b.msg.Content) {
		b.msg.Content[index].Signature = signature
	}
}

// ToolCallStart opens a tool call block with empty arguments.
func (b *Builder) ToolCallStart(id, name string) bool {
	if !b.Start() || !b.EndBlock() {
		return false
	}
	b.openIndex = len(b.msg.Content)
	b.open = blockToolCall
	call := &ToolCall{ID: id, Name: name}
	b.msg.Content = append(b.msg.Content, ContentBlock{Type: ContentTypeToolCall, ToolCall: call})
	return b.emit(Event{Type: EventToolCallStart, Index: b.openIndex, ToolCall: call})
}

// ToolCallEnd closes the current tool call block with its complete arguments; empty arguments become {}.
func (b *Builder) ToolCallEnd(args json.RawMessage) bool {
	if b.open != blockToolCall {
		return !b.stopped
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	call := b.msg.Content[b.openIndex].ToolCall
	call.Arguments = args
	b.open = blockNone
	return b.emit(Event{Type: EventToolCallEnd, Index: b.openIndex, ToolCall: call})
}

// ToolCall appends a complete tool call.
func (b *Builder) ToolCall(id, name string, args json.RawMessage) bool {
	return b.ToolCallStart(id, name) && b.ToolCallEnd(args)
}

func (b *Builder) ensureBlock(kind blockKind) bool {
	if !b.Start() {
		return false
	}
	if b.open == kind {
		return true
	}
	if !b.EndBlock() {
		return false
	}
	b.openIndex = len(b.msg.Content)
	b.open = kind
	if kind == blockText {
		b.msg.Content = append(b.msg.Content, Text(""))
		return b.emit(Event{Type: EventTextStart, Index: b.openIndex})
	}
	b.msg.Content = append(b.msg.Content, ContentBlock{Type: ContentTypeThinking})
	return b.emit(Event{Type: EventThinkingStart, Index: b.openIndex})
}

// Done finishes normally with a done event. An empty reason is inferred from the content.
func (b *Builder) Done(reason StopReason) bool {
	if b.Finished() || !b.Start() || !b.ToolCallEnd(nil) || !b.EndBlock() {
		return false
	}
	switch hasCalls := len(b.msg.ToolCalls()) > 0; {
	case hasCalls && (reason == "" || reason == StopReasonStop):
		reason = StopReasonToolUse
	case reason == "":
		reason = StopReasonStop
	}
	b.msg.StopReason = reason
	b.finished = true
	return b.emit(Event{Type: EventDone, Reason: reason, Message: &b.msg})
}

// Fail finishes with an error event; the message keeps the content generated so far.
// A tool call block that is still open has incomplete arguments and is dropped.
func (b *Builder) Fail(err error) bool {
	if b.Finished() || !b.Start() {
		return false
	}
	if b.open == blockToolCall {
		b.msg.Content = b.msg.Content[:b.openIndex]
		b.open = blockNone
	}
	if !b.EndBlock() {
		return false
	}
	ce := AsError(err)
	reason := StopReasonError
	if ce.Kind == KindAborted {
		reason = StopReasonAborted
	}
	b.msg.StopReason = reason
	b.msg.ErrorMessage = ce.Error()
	b.msg.ErrorKind = ce.Kind
	b.finished = true
	return b.emit(Event{Type: EventError, Reason: reason, Message: &b.msg, Err: ce})
}
