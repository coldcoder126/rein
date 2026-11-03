// Package telegram implements notify.Notifier with the Telegram Bot API.
package telegram

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coldcoder126/rein/x/notify"
)

// Notifier sends messages to a Telegram chat through the Bot API.
type Notifier struct {
	Token  string
	ChatID string
	client *http.Client
}

// New creates a Telegram notifier. token is the Bot API token;
// chatID is the target chat or channel ID (such as "-100123456789").
func New(token, chatID string) *Notifier {
	return &Notifier{
		Token:  token,
		ChatID: chatID,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Notify sends the message to the configured Telegram chat.
func (n *Notifier) Notify(ctx context.Context, msg notify.Message) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.Token)

	form := url.Values{
		"chat_id":    {n.ChatID},
		"text":       {format(msg)},
		"parse_mode": {"Markdown"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("telegram: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram: HTTP %d", resp.StatusCode)
	}
	return nil
}

// format renders the message as a Markdown string.
func format(msg notify.Message) string {
	prefix := levelEmoji(msg.Level)
	var parts []string
	if msg.Title != "" {
		parts = append(parts, fmt.Sprintf("*%s %s*", prefix, escapeMarkdown(msg.Title)))
	}
	if msg.Body != "" {
		parts = append(parts, msg.Body)
	}
	return strings.Join(parts, "\n\n")
}

func levelEmoji(l notify.Level) string {
	switch l {
	case notify.LevelWarn:
		return "⚠️"
	case notify.LevelError:
		return "❌"
	default:
		return "ℹ️"
	}
}

// escapeMarkdown escapes special characters in a Telegram MarkdownV1 title.
func escapeMarkdown(s string) string {
	replacer := strings.NewReplacer("`", "\\`", "_", "\\_", "*", "\\*")
	return replacer.Replace(s)
}
