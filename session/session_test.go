package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
)

func TestFromChat(t *testing.T) {
	assistant := session.FromChat(chat.Message{Role: chat.RoleAssistant, Content: []chat.ContentBlock{
		{Type: chat.ContentTypeThinking, Text: "内心独白"},
		chat.Text("给用户的回答"),
		{Type: chat.ContentTypeToolCall, ToolCall: &chat.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
	}})
	if assistant.Type != session.TypeAssistant || !assistant.Visible || assistant.Text() != "给用户的回答" {
		t.Fatalf("display text should exclude thinking and tool calls: %+v", assistant)
	}
	if len(assistant.Content()) != 3 || assistant.CreatedAt.IsZero() {
		t.Fatalf("the raw message should be kept whole for replay: %+v", assistant.ChatMessage)
	}
	onlyCall := session.FromChat(chat.Message{Role: chat.RoleAssistant, Content: assistant.ChatMessage.Content[2:]})
	toolResult := session.FromChat(chat.Message{Role: chat.RoleTool, Content: []chat.ContentBlock{chat.Text("结果")}})
	if onlyCall.Visible || toolResult.Visible || toolResult.Type != session.TypeTool || toolResult.Kind != session.KindToolResult {
		t.Fatalf("assistant messages with only tool calls and tool results should be hidden by default: %+v %+v", onlyCall, toolResult)
	}
}

func TestMemoryStoreIsolatesCallers(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	if err := store.AppendMessages(ctx, "s1", session.UserMessage(nil, nil)); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a missing session should fail: %v", err)
	}
	if err := store.EnsureSession(ctx, session.Session{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	msg := session.UserMessage([]chat.ContentBlock{chat.Text("原文")}, nil)
	msg.ID = "m1"
	hidden := session.ContextMessage([]chat.ContentBlock{chat.Text("页面状态")}, session.Message{ID: "m2"})
	if summary := session.SummaryMessage("摘要", "m1"); summary.Type != session.TypeSummary || summary.Text() != "摘要" || summary.FirstKeptID != "m1" || summary.Visible {
		t.Fatalf("wrong summary message: %+v", summary)
	}
	if err := store.AppendMessages(ctx, "s1", msg, hidden); err != nil {
		t.Fatal(err)
	}
	msg.Content()[0].Text = "调用方事后修改"
	got, _ := store.ListMessages(ctx, "s1")
	got[0].Content()[0].Text = "读取方修改"
	again, _ := store.ListMessages(ctx, "s1")
	if len(again) != 2 || again[0].Text() != "原文" {
		t.Fatalf("stored content should not be affected by the caller's changes: %+v", again)
	}
	if display := session.DisplayMessages(again); len(display) != 1 || display[0].ID != "m1" {
		t.Fatalf("DisplayMessages should keep only visible messages: %+v", display)
	}
	if s, err := store.GetSession(ctx, "s1"); err != nil || s.CreatedAt.IsZero() {
		t.Fatalf("should get the session with its creation time filled in: %+v %v", s, err)
	}
}

// Session message JSON keys use lower snake case like the inner chat.Message; the outer kind is called type so it does not clash with the inner role.
func TestMessageJSON(t *testing.T) {
	msg := session.SummaryMessage("摘要", "m1")
	msg.ID, msg.SessionID = "m2", "s1"
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "session_id", "type", "chat_message", "visible", "source", "kind", "scope", "first_kept_id", "created_at"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("missing key %q: %s", key, data)
		}
	}
	if len(keys) != 10 {
		t.Errorf("empty details should be omitted: %s", data)
	}
	var back session.Message
	if err := json.Unmarshal(data, &back); err != nil || back.Type != session.TypeSummary || back.FirstKeptID != "m1" || back.Text() != "摘要" {
		t.Fatalf("message read back differs: %+v, %v", back, err)
	}
}
