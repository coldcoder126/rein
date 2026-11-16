package session_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
)

type plainStore struct{ session.Store }

func seed(t *testing.T, store session.Store, id string, messages ...session.Message) {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureSession(ctx, session.Session{ID: id}); err != nil {
		t.Fatal(err)
	}
	for i := range messages {
		messages[i].SessionID = id
	}
	if err := store.AppendMessages(ctx, id, messages...); err != nil {
		t.Fatal(err)
	}
}

func reply(id, s string) session.Message {
	msg := session.FromChat(chat.Message{Role: chat.RoleAssistant, Content: []chat.ContentBlock{chat.Text(s)}})
	msg.ID = id
	return msg
}

func TestForkChecksItsArguments(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	seed(t, store, "s", text("u1", "q"), reply("a1", "a"))
	u1 := text("u1", "q")
	u1.SessionID = "s"
	notice := text("n1", "<task_result>")
	notice.Source = session.SourceSystem
	other := u1
	other.SessionID = "t"
	a1 := reply("a1", "a")
	for name, call := range map[string]func() error{
		"no source":          func() error { return session.Fork(ctx, store, "", session.Session{ID: "f"}, nil) },
		"no new ID":          func() error { return session.Fork(ctx, store, "s", session.Session{}, nil) },
		"into itself":        func() error { return session.Fork(ctx, store, "s", session.Session{ID: "s"}, nil) },
		"assistant message":  func() error { return session.Fork(ctx, store, "s", session.Session{ID: "f"}, &a1) },
		"task notice":        func() error { return session.Fork(ctx, store, "s", session.Session{ID: "f"}, &notice) },
		"another session":    func() error { return session.Fork(ctx, store, "s", session.Session{ID: "f"}, &other) },
		"message without ID": func() error { return session.Fork(ctx, store, "s", session.Session{ID: "f"}, &session.Message{}) },
	} {
		if err := call(); err == nil {
			t.Fatalf("%s: should be rejected", name)
		}
	}
	if err := session.Fork(ctx, plainStore{store}, "s", session.Session{ID: "f"}, nil); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("a store without Forker should return errors.ErrUnsupported: %v", err)
	}
	if _, err := store.GetSession(ctx, "f"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a rejected fork should create nothing: %v", err)
	}

	if err := session.Fork(ctx, store, "s", session.Session{ID: "f"}, &u1); err != nil {
		t.Fatal(err)
	}
	if s, err := store.GetSession(ctx, "f"); err != nil || s.ForkOf != "s" || s.ForkAt != "u1" || s.CreatedAt.IsZero() {
		t.Fatalf("the fork should record where it came from: %+v %v", s, err)
	}
	if err := session.Fork(ctx, store, "s", session.Session{ID: "g", ForkAt: "stale"}, nil); err != nil {
		t.Fatal(err)
	}
	if s, _ := store.GetSession(ctx, "g"); s.ForkOf != "s" || s.ForkAt != "" || len(listMessages(t, store, "g")) != 2 {
		t.Fatalf("a fork without a message should copy the whole session: %+v", s)
	}
}

func listMessages(t *testing.T, store session.Store, id string) []session.Message {
	t.Helper()
	got, err := store.ListMessages(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestForkCutsAtTheInputStart(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	withContext := text("u1", "q1")
	withContext.InputStartID = "c1"
	steer := text("u2", "q2")
	seed(t, store, "s", text("u0", "q0"), reply("a0", "a0"), text("c1", "context"), withContext, steer, reply("a1", "a1"))
	all := listMessages(t, store, "s")

	if err := session.Fork(ctx, store, "s", session.Session{ID: "f1"}, &all[3]); err != nil {
		t.Fatal(err)
	}
	if got := listIDs(t, store, "f1"); got != "u0,a0" {
		t.Fatalf("forking before an input should leave out the context stored with it: %s", got)
	}
	if err := session.Fork(ctx, store, "s", session.Session{ID: "f2"}, &all[4]); err != nil {
		t.Fatal(err)
	}
	if got := listIDs(t, store, "f2"); got != "u0,a0,c1,u1" {
		t.Fatalf("forking before a message without InputStartID should cut at the message itself: %s", got)
	}
}

// A fork keeps message IDs, so a summary copied into it still finds the message it keeps, and a fork before a summary
// starts from the state before that compaction.
func TestForkAcrossACompaction(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	summary := session.SummaryMessage("summary", session.Compaction{FirstKeptID: "u2"})
	summary.ID = "sum"
	seed(t, store, "s", text("u1", "q1"), reply("a1", "a1"), text("u2", "q2"), reply("a2", "a2"), summary, text("u3", "q3"))
	all := listMessages(t, store, "s")

	if err := session.Fork(ctx, store, "s", session.Session{ID: "after"}, &all[5]); err != nil {
		t.Fatal(err)
	}
	after := listMessages(t, store, "after")
	if got := ids(session.Tail(after)); got != "u2,a2,sum" {
		t.Fatalf("the copied summary should still apply: %s", got)
	}
	if got := ids(session.Compacted(after)); got != "u1,a1" {
		t.Fatalf("the replaced messages should be found in the fork: %s", got)
	}

	if err := session.Fork(ctx, store, "s", session.Session{ID: "before"}, &all[2]); err != nil {
		t.Fatal(err)
	}
	before := listMessages(t, store, "before")
	if got := ids(session.Tail(before)); got != "u1,a1" || len(session.Compacted(before)) != 0 {
		t.Fatalf("a fork before the compaction should not have it: %s", got)
	}
}

func ids(messages []session.Message) string {
	var out []string
	for _, msg := range messages {
		out = append(out, msg.ID)
	}
	return strings.Join(out, ",")
}
