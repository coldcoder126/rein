package chat

import (
	"context"
	"errors"
	"iter"
)

// Complete consumes a Client's stream and returns the final assistant message.
// When the model call fails it returns the partial message generated so far and a typed error (*Error).
func Complete(ctx context.Context, c Client, req Request) (*Message, error) {
	return Collect(c.Stream(ctx, req))
}

// Collect consumes an event sequence and returns the final assistant message.
func Collect(events iter.Seq[Event]) (*Message, error) {
	for e := range events {
		switch e.Type {
		case EventDone:
			msg := e.Message.Clone()
			return &msg, nil
		case EventError:
			msg := e.Message.Clone()
			return &msg, e.Err
		}
	}
	return nil, errors.New("chat: stream ended before a terminal event")
}
