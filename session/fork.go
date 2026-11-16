package session

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Fork creates the session dst from the session srcID, which is left unchanged: a branch of the conversation.
//
// With before nil, dst gets all of srcID's messages. Otherwise before is a message the user typed into srcID, and dst gets
// the messages preceding it, without the business context stored as part of the same input (Message.InputStartID); running
// dst with new input then edits that message. Messages keep their IDs, so summaries in the copied part still apply.
// Background tasks and queued messages of srcID stay with it.
//
// dst.ForkOf and dst.ForkAt are filled in. The store must implement Forker, or Fork returns an error wrapping
// errors.ErrUnsupported; it returns ErrExists when dst already exists.
func Fork(ctx context.Context, store Store, srcID string, dst Session, before *Message) error {
	switch {
	case srcID == "" || dst.ID == "":
		return errors.New("session: fork needs a source and a new session ID")
	case srcID == dst.ID:
		return errors.New("session: cannot fork a session into itself")
	}
	forker, ok := store.(Forker)
	if !ok {
		return fmt.Errorf("session: the store cannot fork sessions: %w", errors.ErrUnsupported)
	}
	cutID := ""
	dst.ForkOf, dst.ForkAt = srcID, ""
	if before != nil {
		switch {
		case before.ID == "":
			return errors.New("session: the message to fork before has no ID")
		case before.SessionID != "" && before.SessionID != srcID:
			return fmt.Errorf("session: message %q belongs to session %q, not %q", before.ID, before.SessionID, srcID)
		case before.Type != TypeUser || before.Source != SourceUserInput:
			return fmt.Errorf("session: can only fork before user input, not a %s message from %q", before.Type, before.Source)
		}
		cutID = before.ID
		if before.InputStartID != "" {
			cutID = before.InputStartID
		}
		dst.ForkAt = before.ID
	}
	if dst.CreatedAt.IsZero() {
		dst.CreatedAt = time.Now()
	}
	return forker.ForkSession(ctx, dst, srcID, cutID)
}
