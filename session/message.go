// Package session defines the persistence model of sessions: sessions, session messages and the store interface.
//
// It only describes what is stored and where, not how runs work; when to store and what to store is up to the runner.
// A custom storage backend only needs to depend on this package.
package session

import (
	"strings"
	"time"

	"github.com/coldcoder126/rein/chat"
)

// MessageType is the kind of a session message. It is not the same as ChatMessage.Role, the role used when
// sending to the model: business context and summaries are sent to the model with the user role.
type MessageType string

const (
	TypeUser      MessageType = "user"
	TypeAssistant MessageType = "assistant"
	TypeTool      MessageType = "tool"
	// TypeContext is business context injected for the model, not something the user said.
	TypeContext MessageType = "context"
	// TypeSummary is a summary produced by compaction; it replaces the history before FirstKeptID in the model context.
	TypeSummary MessageType = "summary"
)

// Scope is the lifetime of a message in the session.
type Scope string

const (
	// ScopeRun messages only enter the model context of one run and are not stored, such as temporary context.
	ScopeRun Scope = "run"
	// ScopePersistent messages are stored and stay visible to later runs.
	ScopePersistent Scope = "persistent"
)

// Common Source and Kind values. Both are free-form strings; the business layer may use its own values.
const (
	SourceUserInput = "user_input"
	SourceSystem    = "system"
	SourceAgent     = "agent"
	SourceTool      = "tool"

	KindNormal     = "normal"
	KindToolResult = "tool_result"
	// KindTaskResult is the result notice sent back to the session when a background task (sub-agent, background command and so on) ends; Details is a tool.Task.
	KindTaskResult = "task_result"
)

// Message is a complete session message, as persisted and shown in a UI.
type Message struct {
	ID        string      `json:"id"`
	SessionID string      `json:"session_id"`
	Type      MessageType `json:"type"`
	// RunID is the run during which the message was written to history, for retrieving all messages of a run.
	// Background task results written directly to history while the session is idle belong to no run and have an empty RunID.
	RunID string `json:"run_id,omitempty"`
	// ChatMessage is the single source of the message content. User messages, business context and summaries only use its Content;
	// assistant and tool messages are the raw messages produced by the model, with thinking, tool calls, signatures and usage, replayed as is.
	ChatMessage chat.Message `json:"chat_message"`

	// Visible reports whether the message should be shown to the user.
	Visible bool `json:"visible"`
	// Source and Kind describe where the message came from and what it is, for the business layer to classify; the framework does not depend on their values.
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Scope  Scope  `json:"scope"`
	// Details is structured data attached by the business layer and never sent to the model.
	Details any `json:"details,omitempty"`

	// FirstKeptID is only used by summary messages: messages before this ID are replaced by the summary and no longer enter the model context.
	FirstKeptID string    `json:"first_kept_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Content returns the content blocks of the message.
func (m Message) Content() []chat.ContentBlock { return m.ChatMessage.Content }

// Clone returns a deep copy of the message.
func (m Message) Clone() Message {
	out := m
	out.ChatMessage = m.ChatMessage.Clone()
	return out
}

// Text returns the display text of the message: text blocks only, media as placeholders, no thinking or tool calls.
func (m Message) Text() string {
	var sb strings.Builder
	for _, block := range m.ChatMessage.Content {
		switch block.Type {
		case chat.ContentTypeText:
			sb.WriteString(block.Text)
		case chat.ContentTypeMedia:
			sb.WriteString(mediaLabel(block))
		}
	}
	return sb.String()
}

// CloneMessages returns a deep copy of a list of messages.
func CloneMessages(in []Message) []Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]Message, len(in))
	for i := range in {
		out[i] = in[i].Clone()
	}
	return out
}

// UserMessage creates a user input message.
func UserMessage(content []chat.ContentBlock, details any) Message {
	return contentMessage(TypeUser, content, Message{Visible: true, Source: SourceUserInput, Kind: KindNormal, Details: details})
}

// ContextMessage creates a business context message, taking the other fields from base.
func ContextMessage(content []chat.ContentBlock, base Message) Message {
	return contentMessage(TypeContext, content, base)
}

// SummaryMessage creates a summary message; firstKeptID is the first message kept after the summary.
func SummaryMessage(summary, firstKeptID string) Message {
	return contentMessage(TypeSummary, []chat.ContentBlock{chat.Text(summary)}, Message{Source: SourceSystem, FirstKeptID: firstKeptID})
}

// contentMessage creates a message not produced by the model from content blocks.
func contentMessage(typ MessageType, content []chat.ContentBlock, base Message) Message {
	base.Type = typ
	base.CreatedAt = time.Now()
	base.ChatMessage = chat.Message{Role: chat.RoleUser, Content: content, CreatedAt: base.CreatedAt}
	if base.Scope == "" {
		base.Scope = ScopePersistent
	}
	return base
}

// FromChat turns an assistant or tool message produced by the model into a session message. An assistant message is
// visible only when it has displayable text; tool results are hidden by default.
func FromChat(msg chat.Message) Message {
	out := Message{
		ChatMessage: msg.Clone(),
		Details:     msg.Details,
		Source:      SourceAgent,
		Kind:        KindNormal,
		Scope:       ScopePersistent,
		CreatedAt:   msg.CreatedAt,
	}
	if out.CreatedAt.IsZero() {
		out.CreatedAt = time.Now()
	}
	switch msg.Role {
	case chat.RoleTool:
		out.Type = TypeTool
		out.Source = SourceTool
		out.Kind = KindToolResult
	case chat.RoleUser:
		out.Type = TypeUser
		out.Visible = true
	default:
		out.Type = TypeAssistant
		out.Visible = strings.TrimSpace(out.Text()) != ""
	}
	return out
}

// mediaLabel returns the text placeholder of a media block, such as [media:report.pdf] or [media:image/png].
func mediaLabel(block chat.ContentBlock) string {
	switch {
	case block.Name != "":
		return "[media:" + block.Name + "]"
	case block.MIMEType != "":
		return "[media:" + block.MIMEType + "]"
	}
	return "[media]"
}
