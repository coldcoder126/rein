// Package storetest checks that a session.Store implementation behaves as the runner expects.
// A store's own tests call Run with a function that creates an empty store.
package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
)

// Run runs the conformance checks against stores created by newStore, which must return an empty store each time.
// The checks of optional interfaces (session.TailLister, session.Forker) only run when the store implements them.
func Run(t *testing.T, newStore func(t *testing.T) session.Store) {
	t.Run("Sessions", func(t *testing.T) { testSessions(t, newStore(t)) })
	t.Run("Messages", func(t *testing.T) { testMessages(t, newStore(t)) })
	t.Run("Isolation", func(t *testing.T) { testIsolation(t, newStore(t)) })
	t.Run("RoundTrip", func(t *testing.T) { testRoundTrip(t, newStore(t)) })
	t.Run("SharedIDs", func(t *testing.T) { testSharedIDs(t, newStore(t)) })
	t.Run("Fork", func(t *testing.T) {
		store := newStore(t)
		if _, ok := store.(session.Forker); !ok {
			t.Skip("the store does not implement session.Forker")
		}
		testFork(t, store)
	})
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
	summary := session.SummaryMessage("summary", session.Compaction{FirstKeptID: "m2"})
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
	if got := messages[2]; got.Type != session.TypeSummary || got.Compaction == nil || got.Compaction.FirstKeptID != "m2" || got.Text() != "summary" {
		t.Fatalf("a summary should keep its Compaction: %+v", got)
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

// testRoundTrip stores messages as the runner writes them, with every field the framework relies on for replay, and
// expects them back unchanged. Details is left out: a serializing store may return it as JSON values.
func testRoundTrip(t *testing.T, store session.Store) {
	ctx := context.Background()
	if err := store.EnsureSession(ctx, session.Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 16, 9, 30, 15, 123456789, time.UTC)
	assistant := session.FromChat(chat.Message{
		Role: chat.RoleAssistant,
		Content: []chat.ContentBlock{
			{Type: chat.ContentTypeThinking, Text: "weighing the options", Signature: "sig-1"},
			chat.Text("Here is the chart."),
			{Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: []byte{0x89, 'P', 'N', 'G', 0, 1, 2}, Name: "chart.png"},
			{Type: chat.ContentTypeToolCall, Signature: "sig-2", ToolCall: &chat.ToolCall{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"rein"}`)}},
			{Type: chat.ContentTypeServerTool, Raw: json.RawMessage(`{"type":"tool_search_result","id":"srv_1"}`)},
		},
		Provider: "anthropic", API: "anthropic-messages", Model: "m", ResponseModel: "m-1", ResponseID: "resp_1",
		Usage:      chat.Usage{Input: 1200, Output: 300, TotalTokens: 1500},
		StopReason: chat.StopReasonToolUse,
		CreatedAt:  at,
	})
	assistant.ID, assistant.SessionID, assistant.RunID = "a1", "s", "run_1"
	result := session.FromChat(chat.Message{Role: chat.RoleTool, ToolCallID: "call_1", ToolName: "lookup", IsError: true,
		Content: []chat.ContentBlock{chat.Text("not found")}, CreatedAt: at})
	result.ID, result.SessionID, result.RunID = "t1", "s", "run_1"
	summary := session.SummaryMessage("the gist", session.Compaction{
		FirstKeptID: "a1", Reason: session.CompactionOverflow, EstimatedTokensBefore: 180_000,
		Usage: chat.Usage{Input: 900, Output: 100, TotalTokens: 1000},
	})
	summary.ID, summary.SessionID, summary.RunID, summary.CreatedAt = "s1", "s", "run_2", at
	summary.ChatMessage.CreatedAt = at

	want := []session.Message{assistant, result, summary}
	if err := store.AppendMessages(ctx, "s", want...); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListMessages(ctx, "s")
	if err != nil || len(got) != len(want) {
		t.Fatalf("ListMessages: %s %v", ids(got), err)
	}
	for i := range want {
		w, _ := json.Marshal(want[i])
		g, _ := json.Marshal(got[i])
		if !bytes.Equal(w, g) {
			t.Fatalf("message %s should come back as stored:\nwant %s\ngot  %s", want[i].ID, w, g)
		}
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
		msg := session.SummaryMessage("summary", session.Compaction{FirstKeptID: firstKept})
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

// testSharedIDs checks that message IDs only need to be unique within a session: a fork copies messages with their IDs.
func testSharedIDs(t *testing.T, store session.Store) {
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := store.EnsureSession(ctx, session.Session{ID: id}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessages(ctx, id, text("m1", id)); err != nil {
			t.Fatalf("the same message ID should be accepted in another session: %v", err)
		}
	}
	for _, id := range []string{"a", "b"} {
		got, err := store.ListMessages(ctx, id)
		if err != nil || len(got) != 1 || got[0].Text() != id {
			t.Fatalf("session %s should keep its own message m1: %+v %v", id, got, err)
		}
	}
}

func testFork(t *testing.T, store session.Store) {
	ctx := context.Background()
	forker := store.(session.Forker)
	list := func(id string) []session.Message {
		t.Helper()
		got, err := store.ListMessages(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if err := store.EnsureSession(ctx, session.Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	var src []session.Message
	for _, id := range []string{"m1", "m2", "m3"} {
		msg := text(id, id)
		msg.SessionID = "s"
		src = append(src, msg)
	}
	if err := store.AppendMessages(ctx, "s", src...); err != nil {
		t.Fatal(err)
	}

	dst := session.Session{ID: "f", ForkOf: "s", ForkAt: "m3", CreatedAt: time.Now()}
	if err := forker.ForkSession(ctx, dst, "s", "m2"); err != nil {
		t.Fatal(err)
	}
	got := list("f")
	if ids(got) != "[m1]" || got[0].SessionID != "f" || got[0].Text() != "m1" {
		t.Fatalf("the fork should hold the messages before the cut, with its own SessionID: %+v", got)
	}
	if s, err := store.GetSession(ctx, "f"); err != nil || s.ForkOf != "s" || s.ForkAt != "m3" {
		t.Fatalf("the fork's metadata should read back: %+v %v", s, err)
	}
	if got := list("s"); ids(got) != "[m1 m2 m3]" || got[0].SessionID != "s" {
		t.Fatalf("the source should be unchanged: %+v", got)
	}
	if err := store.AppendMessages(ctx, "f", text("m2", "edited")); err != nil {
		t.Fatal(err)
	}
	if got := list("f"); ids(got) != "[m1 m2]" || got[1].Text() != "edited" {
		t.Fatalf("the fork should take its own messages: %+v", got)
	}
	if got := list("s"); ids(got) != "[m1 m2 m3]" || got[1].Text() != "m2" {
		t.Fatalf("appending to the fork should not change the source: %+v", got)
	}

	if err := forker.ForkSession(ctx, session.Session{ID: "all"}, "s", ""); err != nil {
		t.Fatal(err)
	}
	if got := list("all"); ids(got) != "[m1 m2 m3]" {
		t.Fatalf("an empty cut should copy every message: %+v", got)
	}
	if err := forker.ForkSession(ctx, session.Session{ID: "head"}, "s", "m1"); err != nil {
		t.Fatal(err)
	}
	if got := list("head"); len(got) != 0 {
		t.Fatalf("cutting at the first message should leave the fork empty: %+v", got)
	}

	for _, existing := range []string{"f", "s"} {
		if err := forker.ForkSession(ctx, session.Session{ID: existing}, "s", ""); !errors.Is(err, session.ErrExists) {
			t.Fatalf("forking into the existing session %s should return session.ErrExists: %v", existing, err)
		}
	}
	if got := list("f"); ids(got) != "[m1 m2]" {
		t.Fatalf("a failed fork should not change the existing session: %+v", got)
	}
	for _, c := range []struct{ src, cut string }{{"missing", ""}, {"s", "missing"}} {
		err := forker.ForkSession(ctx, session.Session{ID: "x"}, c.src, c.cut)
		if !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("forking %+v should return session.ErrNotFound: %v", c, err)
		}
		if _, err := store.GetSession(ctx, "x"); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("a failed fork should not create the session: %v", err)
		}
	}

	// Appending to the source while forking it does not change the prefix before the cut.
	done := make(chan error, 1)
	go func() { done <- store.AppendMessages(ctx, "s", text("m4", "m4"), text("m5", "m5")) }()
	if err := forker.ForkSession(ctx, session.Session{ID: "race"}, "s", "m3"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := list("race"); ids(got) != "[m1 m2]" {
		t.Fatalf("a fork made while the source grows should hold the prefix: %+v", got)
	}
}
