// Package notify provides a common interface for sending Agent notifications.
package notify

import "context"

// Level is the severity of a notification.
type Level int

const (
	LevelInfo  Level = iota // information
	LevelWarn               // warning
	LevelError              // error / alert
)

// Message is the body of a notification.
type Message struct {
	Title string
	Body  string
	Level Level
}

// Notifier sends notifications to external channels (Telegram, webhooks and so on).
type Notifier interface {
	Notify(ctx context.Context, msg Message) error
}

// Multi fans a notification out to several Notifiers.
// On an error it records the first one and keeps calling the remaining Notifiers, finally returning the first error.
type Multi struct {
	Notifiers []Notifier
}

func (m *Multi) Notify(ctx context.Context, msg Message) error {
	var first error
	for _, n := range m.Notifiers {
		if err := n.Notify(ctx, msg); err != nil && first == nil {
			first = err
		}
	}
	return first
}
