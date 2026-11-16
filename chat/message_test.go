package chat_test

import (
	"encoding/json"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

// A clone's blocks can be changed on their own, while the read-only bytes they point to are shared, not copied.
func TestCloneSharesBytes(t *testing.T) {
	msg := chat.Message{Role: chat.RoleAssistant, Content: []chat.ContentBlock{
		chat.Text("hi"),
		{Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: []byte("png")},
		{Type: chat.ContentTypeToolCall, ToolCall: &chat.ToolCall{ID: "c1", Name: "f", Arguments: json.RawMessage(`{}`)}},
		{Type: chat.ContentTypeServerTool, Name: "s", Raw: json.RawMessage(`{"type":"x"}`)},
	}}
	clone := msg.Clone()
	if &clone.Content[1].Data[0] != &msg.Content[1].Data[0] || &clone.Content[3].Raw[0] != &msg.Content[3].Raw[0] ||
		&clone.Content[2].ToolCall.Arguments[0] != &msg.Content[2].ToolCall.Arguments[0] {
		t.Fatal("a clone should share the content bytes")
	}
	clone.Content[0].Text = "changed"
	clone.Content[2].ToolCall.Name = "g"
	clone.Content = append(clone.Content, chat.Text("more"))
	if msg.Content[0].Text != "hi" || msg.Content[2].ToolCall.Name != "f" || len(msg.Content) != 4 {
		t.Fatalf("changing a clone's blocks should not change the original: %+v", msg.Content)
	}
}
