// Package storetest checks that a session.Store implementation behaves as the runner expects.
// A store's own tests call Run with a function that creates an empty store.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
)

// Run runs the conformance checks against stores created by newStore, which must return an empty store each time.
// The checks of optional interfaces (session.TailLister) only run when the store implements them.
func Run(t *testing.T, newStore func(t *testing.T) session.Store) {
	t.Run("Sessions", func(t *testing.T) { testSessions(t, newStore(t)) })
	t.Run("Messages", func(t *testing.T) { testMessages(t, newStore(t)) })
	t.Run("Isolation", func(t *testing.T) { testIsolation(t, newStore(t)) })
	t.Run("Tail", func(t *testing.T) {
		store := newStore(t)
		if _, ok := store.(session.TailLister); !ok {
			t.Skip("the store does not implement session.TailLister")
		}
		testTail(t, store)
	})
}

func text(id, s string) session.Message {
	msg := session.UserMessage([]chat.ContentBlock{chat.Text(s)}, nil)
	msg.ID = id
	return msg
}

func ids(messages []session.Message) string {
	var out []string
	for _, msg := range messages {
		out = append(out, msg.ID)
	}
	return fmt.Sprint(out)
}

func testSessions(t *testing.T, store session.Store) {
	ctx := context.Background()
	if _, err := store.GetSession(ctx, "missing"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("GetSession of a missing session should return session.ErrNotFound: %v", err)
	}
	if _, err := store.ListMessages(ctx, "missing"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("ListMessages of a missing session should return session.ErrNotFound: %v", err)
	}
	if err := store.AppendMessages(ctx, "missing", text("m1", "a")); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("AppendMessages to a missing session should return session.ErrNotFound: %v", err)
	}
	if err := store.EnsureSession(ctx, session.Session{ID: "s", ParentID: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessages(ctx, "s", text("m1", "a")); err != nil {
		t.Fatal(err)
	}
	// Ensuring an existing session changes nothing: neither its metadata nor its messages.
	if err := store.EnsureSession(ctx, session.Session{ID: "s", ParentID: "other"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSession(ctx, "s")
	if err != nil || got.ID != "s" || got.ParentID != "p" || got.CreatedAt.IsZero() {
		t.Fatalf("the session should keep its first metadata and have a creation time: %+v %v", got, err)
	}
	if messages, err := store.ListMessages(ctx, "s"); err != nil || len(messages) != 1 {
		t.Fatalf("ensuring an existing session should keep its messages: %s %v", ids(messages), err)
	}
}

func testMessages(t *testing.T, store session.Store) {
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := store.EnsureSession(ctx, session.Session{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if messages, err := store.ListMessages(ctx, "a"); err != nil || len(messages) != 0 {
		t.Fatalf("a new session should have no messages: %s %v", ids(messages), err)
	}
	if err := store.AppendMessages(ctx, "a", text("m1", "one"), text("m2", "two")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessages(ctx, "b", text("x1", "other")); err != nil {
		t.Fatal(err)
	}
	summary := session.SummaryMessage("summary", "m2")
	summary.ID = "m3"
	if err := store.AppendMessages(ctx, "a", summary); err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListMessages(ctx, "a")
	if err != nil || ids(messages) != "[m1 m2 m3]" {
		t.Fatalf("messages should come back in append order, per session: %s %v", ids(messages), err)
	}
	if messages[0].Text() != "one" || messages[0].Type != session.TypeUser || !messages[0].Visible {
		t.Fatalf("a message should come back as stored: %+v", messages[0])
	}
	if got := messages[2]; got.Type != session.TypeSummary || got.FirstKeptID != "m2" || got.Text() != "summary" {
		t.Fatalf("a summary should keep its FirstKeptID: %+v", got)
	}
}

func testIsolation(t *testing.T, store session.Store) {
	ctx := context.Background()
	if err := store.EnsureSession(ctx, session.Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	msg := text("m1", "original")
	if err := store.AppendMessages(ctx, "s", msg); err != nil {
		t.Fatal(err)
	}
	msg.Content()[0].Text = "changed by the writer"
	first, err := store.ListMessages(ctx, "s")
	if err != nil || len(first) != 1 {
		t.Fatalf("ListMessages: %s %v", ids(first), err)
	}
	first[0].Content()[0].Text = "changed by a reader"
	again, err := store.ListMessages(ctx, "s")
	if err != nil || len(again) != 1 || again[0].Text() != "original" {
		t.Fatalf("stored messages should not share memory with the caller's: %+v %v", again, err)
	}
}

func testTail(t *testing.T, store session.Store) {
	ctx := context.Background()
	tail := store.(session.TailLister)
	if _, err := tail.ListTail(ctx, "missing"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("ListTail of a missing session should return session.ErrNotFound: %v", err)
	}
	if err := store.EnsureSession(ctx, session.Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		all, err := store.ListMessages(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		got, err := tail.ListTail(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		if ids(got) != want || ids(got) != ids(session.Tail(all)) {
			t.Fatalf("ListTail should return %s (session.Tail gives %s), got %s", want, ids(session.Tail(all)), ids(got))
		}
	}
	summary := func(id, firstKept string) session.Message {
		msg := session.SummaryMessage("summary", firstKept)
		msg.ID = id
		return msg
	}
	check("[]")
	if err := store.AppendMessages(ctx, "s", text("m1", "1"), text("m2", "2"), text("m3", "3")); err != nil {
		t.Fatal(err)
	}
	check("[m1 m2 m3]")
	if err := store.AppendMessages(ctx, "s", summary("s1", "m2"), text("m4", "4")); err != nil {
		t.Fatal(err)
	}
	check("[m2 m3 s1 m4]")
	// The latest summary decides, and the earlier one stays in the tail when it lies in the kept part.
	if err := store.AppendMessages(ctx, "s", summary("s2", "m3"), text("m5", "5")); err != nil {
		t.Fatal(err)
	}
	check("[m3 s1 m4 s2 m5]")
	// A summary whose kept message is unknown starts the tail itself.
	if err := store.AppendMessages(ctx, "s", summary("s3", "gone"), text("m6", "6")); err != nil {
		t.Fatal(err)
	}
	check("[s3 m6]")
}
