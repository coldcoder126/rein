package anthropic

import (
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

func TestBuildParamsCacheBreakpoints(t *testing.T) {
	client := mustNew(Config{Model: sonnet55})
	req := chat.Request{
		SystemPrompt: "系统提示",
		Messages: []chat.Message{
			chat.NewUserTextMessage("查"),
			{Role: chat.RoleAssistant, Content: []chat.ContentBlock{toolCallBlock("c1", "lookup", `{}`)}},
			{Role: chat.RoleTool, ToolCallID: "c1", Content: []chat.ContentBlock{chat.Text("结果")}},
		},
	}
	got := paramsJSON(t, client, req)
	if strings.Count(got, `"cache_control":{"type":"ephemeral"}`) != 2 {
		t.Fatalf("should put one breakpoint on the system prompt and one on the last block: %s", got)
	}
	if !strings.Contains(got, `"cache_control":{"type":"ephemeral"},"content":[{"text":"结果"`) {
		t.Fatalf("the message breakpoint should be on the last tool result: %s", got)
	}

	req.CacheRetention = chat.CacheRetentionLong
	if got := paramsJSON(t, client, req); strings.Count(got, `"ttl":"1h"`) != 2 {
		t.Fatalf("long should use a 1 hour TTL: %s", got)
	}
	req.CacheRetention = chat.CacheRetentionNone
	if got := paramsJSON(t, client, req); strings.Contains(got, "cache_control") {
		t.Fatalf("none should send no cache breakpoints: %s", got)
	}
}
