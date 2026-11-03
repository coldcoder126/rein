package runner

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
)

// MessageConverter converts session messages into messages visible to the model.
//
// It also converts queued steer / follow-up messages, which can only become user messages (possibly merged into fewer messages).
type MessageConverter interface {
	ToChatMessages(ctx context.Context, messages []session.Message) ([]chat.Message, error)
}

// DefaultConverter is the default message converter: consecutive user messages and business context are merged into one model message,
// business context is wrapped in <app_context>, and summaries in <conversation_summary>.
type DefaultConverter struct{}

func (DefaultConverter) ToChatMessages(_ context.Context, messages []session.Message) ([]chat.Message, error) {
	out := make([]chat.Message, 0, len(messages))
	var user userMessageBuilder
	for _, msg := range messages {
		switch msg.Type {
		case session.TypeUser:
			user.appendMessage(msg.ChatMessage)
		case session.TypeContext, session.TypeSummary:
			text := msg.Text()
			if strings.TrimSpace(text) == "" {
				continue
			}
			open := "<app_context"
			if msg.Source != "" {
				open += fmt.Sprintf(" source=\"%s\"", xmlEscape(msg.Source))
			}
			if msg.Kind != "" {
				open += fmt.Sprintf(" kind=\"%s\"", xmlEscape(msg.Kind))
			}
			wrapped := open + ">\n" + text + "\n</app_context>"
			if msg.Type == session.TypeSummary {
				wrapped = "<conversation_summary>\n" + text + "\n</conversation_summary>"
			}
			user.appendMessage(chat.Message{Role: chat.RoleUser, Content: []chat.ContentBlock{chat.Text(wrapped)}, CreatedAt: msg.CreatedAt})
		case session.TypeAssistant, session.TypeTool:
			out = user.flush(out)
			out = append(out, msg.ChatMessage.Clone())
		}
	}
	return user.flush(out), nil
}

type userMessageBuilder struct {
	content   []chat.ContentBlock
	createdAt time.Time
}

func (b *userMessageBuilder) appendMessage(msg chat.Message) {
	if len(msg.Content) == 0 {
		return
	}
	if len(b.content) > 0 {
		b.content = append(b.content, chat.Text("\n\n"))
	}
	b.content = append(b.content, chat.CloneBlocks(msg.Content)...)
	if b.createdAt.IsZero() || (!msg.CreatedAt.IsZero() && msg.CreatedAt.Before(b.createdAt)) {
		b.createdAt = msg.CreatedAt
	}
}

func (b *userMessageBuilder) flush(out []chat.Message) []chat.Message {
	if len(b.content) == 0 {
		return out
	}
	out = append(out, chat.Message{
		Role:      chat.RoleUser,
		Content:   chat.CloneBlocks(b.content),
		CreatedAt: b.createdAt,
	})
	b.content = nil
	b.createdAt = time.Time{}
	return out
}

func xmlEscape(value string) string {
	var sb strings.Builder
	_ = xml.EscapeText(&sb, []byte(value))
	return sb.String()
}
