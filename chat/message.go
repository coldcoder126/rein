// Package chat defines the lowest-level LLM chat abstraction.
package chat

import (
	"encoding/json"
	"strings"
	"time"
)

// Role is the role of a conversation message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentType is the type of a message content block.
type ContentType string

const (
	ContentTypeText ContentType = "text"
	// ContentTypeMedia is media content such as images, documents, audio and video; see Media and MediaURL.
	ContentTypeMedia    ContentType = "media"
	ContentTypeThinking ContentType = "thinking"
	ContentTypeToolCall ContentType = "tool_call"
)

// ContentBlock is a message content block. The blocks of an assistant message keep the exact order returned by the model and are restored unchanged when sent back.
type ContentBlock struct {
	Type ContentType `json:"type"`
	// Text is the text of a text or thinking block.
	Text string `json:"text,omitempty"`
	// Signature is the replayable signature of a thinking block; a redacted thinking block stores its encrypted payload here.
	Signature string `json:"signature,omitempty"`
	// Redacted marks a thinking block hidden by safety policy; only Signature is meaningful.
	Redacted bool `json:"redacted,omitempty"`
	// The next five fields describe a media block: MIMEType decides how the model handles it, the content comes from
	// File (a file uploaded to the provider), Data (inline) or URL, and Name is an optional file name.
	// File is used when the target model can reach it; otherwise Data or URL is the fallback, see NormalizeForModel.
	MIMEType string   `json:"mime_type,omitempty"`
	Data     []byte   `json:"data,omitempty"`
	URL      string   `json:"url,omitempty"`
	File     *FileRef `json:"file,omitempty"`
	Name     string   `json:"name,omitempty"`
	// ToolCall is the content of a tool call block.
	ToolCall *ToolCall `json:"tool_call,omitempty"`
}

// Text creates a text content block.
func Text(text string) ContentBlock {
	return ContentBlock{Type: ContentTypeText, Text: text}
}

// ToolCall is a tool call requested by the model.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolDef is a tool definition exposed to the model.
type ToolDef struct {
	Name          string           `json:"name"`
	Description   string           `json:"description,omitempty"`
	InputSchema   json.RawMessage  `json:"input_schema,omitempty"`
	InputExamples []map[string]any `json:"input_examples,omitempty"`
}

// StopReason is why the model stopped generating.
type StopReason string

const (
	StopReasonStop    StopReason = "stop"
	StopReasonLength  StopReason = "length"
	StopReasonToolUse StopReason = "tool_use"
	StopReasonError   StopReason = "error"
	StopReasonAborted StopReason = "aborted"
)

// Message is a provider-neutral chat message.
//
// A user message's Content may mix text and media; an assistant message's Content may mix text, thinking, tool calls and generated images;
// a tool result message uses ToolCallID, ToolName, IsError and Details.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content,omitempty"`

	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	Details    any    `json:"details,omitempty"`

	// The following fields are only meaningful on assistant messages. Provider is the vendor that served the reply,
	// API the protocol it came over (see ModelInfo.API), Model the model asked for and ResponseModel the one that answered.
	Provider      string     `json:"provider,omitempty"`
	API           string     `json:"api,omitempty"`
	Model         string     `json:"model,omitempty"`
	ResponseModel string     `json:"response_model,omitempty"`
	ResponseID    string     `json:"response_id,omitempty"`
	Usage         Usage      `json:"usage,omitzero"`
	StopReason    StopReason `json:"stop_reason,omitempty"`
	ErrorMessage  string     `json:"error_message,omitempty"`
	ErrorKind     ErrorKind  `json:"error_kind,omitempty"`

	CreatedAt time.Time `json:"created_at,omitzero"`
}

// NewUserTextMessage creates a user text message.
func NewUserTextMessage(text string) Message {
	return Message{
		Role:      RoleUser,
		Content:   []ContentBlock{Text(text)},
		CreatedAt: time.Now(),
	}
}

// TextContent joins the text blocks of the message, for logs and tests.
func (m Message) TextContent() string {
	var sb strings.Builder
	for _, block := range m.Content {
		if block.Type == ContentTypeText {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}

// ToolCalls returns the tool calls in the message, in order.
func (m Message) ToolCalls() []ToolCall {
	var calls []ToolCall
	for _, block := range m.Content {
		if block.Type == ContentTypeToolCall && block.ToolCall != nil {
			calls = append(calls, *block.ToolCall)
		}
	}
	return calls
}

// Clone returns a deep copy of the message.
func (m Message) Clone() Message {
	out := m
	out.Content = CloneBlocks(m.Content)
	return out
}

// CloneBlocks returns a deep copy of the content blocks.
func CloneBlocks(in []ContentBlock) []ContentBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]ContentBlock, len(in))
	for i, block := range in {
		out[i] = block
		out[i].Data = append([]byte(nil), block.Data...)
		if block.ToolCall != nil {
			call := *block.ToolCall
			call.Arguments = append(json.RawMessage(nil), block.ToolCall.Arguments...)
			out[i].ToolCall = &call
		}
	}
	return out
}
