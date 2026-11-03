package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
)

func TestDefaultConverterMergesContextAndUser(t *testing.T) {
	messages := []session.Message{
		ContextInput{
			Text:   "当前股票：AAPL",
			Source: "ui",
			Kind:   "page_state",
		}.message(),
		session.UserMessage([]chat.ContentBlock{chat.Text("这家公司便宜吗？")}, nil),
	}

	out, err := (DefaultConverter{}).ToChatMessages(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("context + user should merge into one model message, got: %d", len(out))
	}
	if out[0].Role != chat.RoleUser {
		t.Fatalf("wrong role after merging: %s", out[0].Role)
	}
	text := out[0].TextContent()
	if !strings.Contains(text, "<app_context") || !strings.Contains(text, "当前股票：AAPL") || !strings.Contains(text, "这家公司便宜吗？") {
		t.Fatalf("wrong merged content: %q", text)
	}
}

func TestDefaultConverterMergesConsecutiveUsers(t *testing.T) {
	messages := []session.Message{
		session.UserMessage([]chat.ContentBlock{chat.Text("第二问")}, nil),
		session.UserMessage([]chat.ContentBlock{chat.Text("第三问")}, nil),
	}

	out, err := (DefaultConverter{}).ToChatMessages(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("consecutive user messages should merge into one model message, got: %d", len(out))
	}
	if got := out[0].TextContent(); got != "第二问\n\n第三问" {
		t.Fatalf("wrong merged text: %q", got)
	}
}

func TestDefaultConverterFlushesUserBeforeAssistantAndTool(t *testing.T) {
	toolMsg := chat.Message{
		Role:       chat.RoleTool,
		ToolCallID: "call_1",
		ToolName:   "lookup",
		Content:    []chat.ContentBlock{chat.Text("工具结果")},
	}
	messages := []session.Message{
		session.UserMessage([]chat.ContentBlock{chat.Text("第一问")}, nil),
		session.FromChat(chat.Message{
			Role:    chat.RoleAssistant,
			Content: []chat.ContentBlock{chat.Text("助手回答")},
		}),
		session.FromChat(toolMsg),
		session.UserMessage([]chat.ContentBlock{chat.Text("第二问")}, nil),
	}

	out, err := (DefaultConverter{}).ToChatMessages(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("assistant/tool should break user merging, got %d messages", len(out))
	}
	if out[0].Role != chat.RoleUser || out[0].TextContent() != "第一问" {
		t.Fatalf("wrong first user message: %+v", out[0])
	}
	if out[1].Role != chat.RoleAssistant || out[1].TextContent() != "助手回答" {
		t.Fatalf("wrong assistant message: %+v", out[1])
	}
	if out[2].Role != chat.RoleTool || out[2].ToolCallID != "call_1" {
		t.Fatalf("tool messages should keep their original fields: %+v", out[2])
	}
	if out[3].Role != chat.RoleUser || out[3].TextContent() != "第二问" {
		t.Fatalf("wrong second user message: %+v", out[3])
	}
}
