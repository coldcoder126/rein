package anthropic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
)

var searchTools = []chat.ToolDef{
	{Name: "quote", Description: "股价", InputSchema: json.RawMessage(`{"type":"object"}`)},
	{Name: "report", Description: "财报", InputSchema: json.RawMessage(`{"type":"object"}`), Deferred: true},
}

func toolsJSON(t *testing.T, client *Client, req chat.Request) []map[string]any {
	t.Helper()
	params, err := client.buildParams(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body.Tools
}

func TestDeferredToolsUseToolSearch(t *testing.T) {
	req := chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}, Tools: searchTools}

	tools := toolsJSON(t, New(Config{Model: ModelClaudeSonnet5_5}), req)
	if len(tools) != 3 || tools[0]["name"] != "quote" || tools[1]["name"] != "report" {
		t.Fatalf("own tools should keep their order with the search tool appended: %v", tools)
	}
	if _, ok := tools[0]["defer_loading"]; ok || tools[1]["defer_loading"] != true {
		t.Fatalf("only the deferred tool should be marked: %v", tools)
	}
	if tools[2]["type"] != "tool_search_tool_regex_20251119" {
		t.Fatalf("regex search should be the default: %v", tools[2])
	}

	tools = toolsJSON(t, New(Config{Model: ModelClaudeSonnet5_5, ToolSearch: ToolSearchBM25}), req)
	if tools[2]["type"] != "tool_search_tool_bm25_20251119" {
		t.Fatalf("BM25 search was asked for: %v", tools[2])
	}
}

func TestToolSearchIsSkippedWhenUnavailable(t *testing.T) {
	req := chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}, Tools: searchTools}
	for name, client := range map[string]*Client{
		"off":              New(Config{Model: ModelClaudeSonnet5_5, ToolSearch: ToolSearchOff}),
		"non-claude":       New(Config{Model: "deepseek-v4-pro", Endpoint: Endpoint{BaseURL: "http://example.invalid"}}),
		"older model":      New(Config{Model: "claude-3-7-sonnet"}),
		"nothing to defer": New(Config{Model: ModelClaudeSonnet5_5}),
	} {
		r := req
		if name == "nothing to defer" {
			r.Tools = searchTools[:1]
		}
		tools := toolsJSON(t, client, r)
		if len(tools) != len(r.Tools) {
			t.Fatalf("%s: no search tool expected: %v", name, tools)
		}
		for _, tool := range tools {
			if _, ok := tool["defer_loading"]; ok {
				t.Fatalf("%s: tools should load up front: %v", name, tools)
			}
		}
	}
}

const (
	searchCallRaw   = `{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{"query":"财报"},"caller":{"type":"direct"}}`
	searchResultRaw = `{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"report"}]}}`
)

func searchHistory() []chat.Message {
	return []chat.Message{
		chat.NewUserTextMessage("看看财报"),
		{
			Role: chat.RoleAssistant, Provider: providerName, API: API, Model: ModelClaudeSonnet5_5, StopReason: chat.StopReasonToolUse,
			Content: []chat.ContentBlock{
				{Type: chat.ContentTypeServerTool, Name: "tool_search_tool_bm25", Raw: json.RawMessage(searchCallRaw)},
				{Type: chat.ContentTypeServerTool, Name: "tool_search_tool_result", Raw: json.RawMessage(searchResultRaw)},
				toolCallBlock("toolu_1", "report", `{}`),
			},
		},
		{Role: chat.RoleTool, ToolCallID: "toolu_1", ToolName: "report", Content: []chat.ContentBlock{chat.Text("ok")}},
	}
}

func TestServerToolRecordsAreReplayedVerbatim(t *testing.T) {
	params, err := New(Config{Model: ModelClaudeSonnet5_5}).buildParams(chat.Request{Messages: searchHistory(), Tools: searchTools})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(params.Messages[1])
	var msg struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 3 || string(msg.Content[0]) != searchCallRaw || string(msg.Content[1]) != searchResultRaw {
		t.Fatalf("server tool records should be sent back unchanged: %s", raw)
	}

	// Without tool search the records cannot be resolved and are left out; the tool call itself stays.
	params, err = New(Config{Model: ModelClaudeSonnet5_5, ToolSearch: ToolSearchOff}).buildParams(chat.Request{Messages: searchHistory(), Tools: searchTools})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(params.Messages[1].Content); n != 1 {
		t.Fatalf("only the tool call should remain, got %d blocks", n)
	}
}

func TestStreamKeepsServerToolRecords(t *testing.T) {
	client := newTestClient(t, serveSSE(sse(
		ev("message_start", messageStart),
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{},"caller":{"type":"direct"}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"财报\"}"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":`+searchResultRaw+`}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":1}`),
		ev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"report","input":{}}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":2}`),
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`),
		ev("message_stop", `{"type":"message_stop"}`),
	)))
	events := chattest.Record(client.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	msg := events[len(events)-1].Message
	if len(msg.Content) != 3 || msg.Content[0].Type != chat.ContentTypeServerTool || msg.Content[1].Type != chat.ContentTypeServerTool {
		t.Fatalf("wrong content: %+v", msg.Content)
	}
	if msg.Content[0].Name != "tool_search_tool_bm25" || !strings.Contains(string(msg.Content[0].Raw), `"input":{"query":"财报"}`) {
		t.Fatalf("the call should carry its streamed input: %s", msg.Content[0].Raw)
	}
	if msg.Content[1].Name != "tool_search_tool_result" || !strings.Contains(string(msg.Content[1].Raw), `"tool_name":"report"`) {
		t.Fatalf("wrong search result: %s", msg.Content[1].Raw)
	}
	if calls := msg.ToolCalls(); len(calls) != 1 || calls[0].Name != "report" {
		t.Fatalf("the found tool should be called: %+v", calls)
	}
}
