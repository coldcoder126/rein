package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

var searchTools = []chat.ToolDef{
	{Name: "quote", Description: "股价", InputSchema: json.RawMessage(`{"type":"object"}`)},
	{Name: "report", Description: "财报", InputSchema: json.RawMessage(`{"type":"object"}`), Deferred: true},
}

func paramsBody(t *testing.T, c *Client, req chat.Request) struct {
	Tools []map[string]any  `json:"tools"`
	Input []json.RawMessage `json:"input"`
} {
	t.Helper()
	var body struct {
		Tools []map[string]any  `json:"tools"`
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(marshal(t, mustParams(t, c, req))), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDeferredToolsUseHostedToolSearch(t *testing.T) {
	req := chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}, Tools: searchTools}
	tools := paramsBody(t, mustNew(Config{Model: ModelGPT6Luna}), req).Tools
	if len(tools) != 3 || tools[0]["name"] != "quote" || tools[1]["name"] != "report" || tools[2]["type"] != "tool_search" {
		t.Fatalf("own tools should keep their order with tool_search appended: %v", tools)
	}
	if _, ok := tools[0]["defer_loading"]; ok || tools[1]["defer_loading"] != true {
		t.Fatalf("only the deferred tool should be marked: %v", tools)
	}

	for name, client := range map[string]*Client{
		"disabled": mustNew(Config{Model: ModelGPT6Luna, Endpoint: Endpoint{Compat: Compat{NoToolSearch: true}}}),
	} {
		tools := paramsBody(t, client, req).Tools
		if len(tools) != 2 || tools[1]["defer_loading"] != nil {
			t.Fatalf("%s: tools should load up front: %v", name, tools)
		}
	}
	if tools := paramsBody(t, mustNew(Config{Model: ModelGPT6Luna}), chat.Request{Tools: searchTools[:1]}).Tools; len(tools) != 1 {
		t.Fatalf("nothing deferred, no tool_search: %v", tools)
	}
}

const (
	searchCallRaw   = `{"type":"tool_search_call","id":"tsc_1","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["report"]}}`
	searchOutputRaw = `{"type":"tool_search_output","id":"tso_1","execution":"server","call_id":null,"status":"completed","tools":[{"type":"function","name":"report","parameters":{"type":"object"},"defer_loading":true}]}`
)

func TestToolSearchItemsAreReplayedVerbatim(t *testing.T) {
	history := []chat.Message{
		chat.NewUserTextMessage("看看财报"),
		{
			Role: chat.RoleAssistant, Provider: providerName, API: API, Model: ModelGPT6Luna, StopReason: chat.StopReasonToolUse,
			Content: []chat.ContentBlock{
				{Type: chat.ContentTypeServerTool, Name: "tool_search_call", Raw: json.RawMessage(searchCallRaw)},
				{Type: chat.ContentTypeServerTool, Name: "tool_search_output", Raw: json.RawMessage(searchOutputRaw)},
				toolCallBlock("call_1", "report", `{}`, "fc_1"),
			},
		},
		{Role: chat.RoleTool, ToolCallID: "call_1", ToolName: "report", Content: []chat.ContentBlock{chat.Text("ok")}},
	}
	input := paramsBody(t, mustNew(Config{Model: ModelGPT6Luna}), chat.Request{Messages: history, Tools: searchTools}).Input
	if len(input) != 5 || string(input[1]) != searchCallRaw || string(input[2]) != searchOutputRaw {
		t.Fatalf("search items should be sent back unchanged in place: %s", input)
	}

	input = paramsBody(t, mustNew(Config{Model: ModelGPT6Luna, Endpoint: Endpoint{Compat: Compat{NoToolSearch: true}}}), chat.Request{Messages: history, Tools: searchTools}).Input
	for _, item := range input {
		if strings.Contains(string(item), "tool_search") {
			t.Fatalf("without tool search the items are left out: %s", input)
		}
	}
}

func TestStreamKeepsToolSearchItems(t *testing.T) {
	client := newTestClient(t, Config{Model: ModelGPT6Luna}, serveEvents(
		ev("response.created", created),
		ev("response.output_item.done", `"output_index":0,"item":`+searchCallRaw),
		ev("response.output_item.done", `"output_index":1,"item":`+searchOutputRaw),
		ev("response.output_item.done", `"output_index":2,"item":`+callRaw),
		completed(searchCallRaw+","+searchOutputRaw+","+callRaw),
	))
	events := run(t, client)
	msg := events[len(events)-1].Message
	if len(msg.Content) != 3 || msg.Content[0].Type != chat.ContentTypeServerTool || msg.Content[1].Type != chat.ContentTypeServerTool {
		t.Fatalf("wrong content: %+v", msg.Content)
	}
	if msg.Content[0].Name != "tool_search_call" || string(msg.Content[1].Raw) != searchOutputRaw {
		t.Fatalf("items should be kept whole: %+v", msg.Content[:2])
	}
}

func TestLoadedFunctionCallKeepsItsNamespace(t *testing.T) {
	const loadedCall = `{"id":"fc_9","type":"function_call","call_id":"call_9","namespace":"report","name":"report","arguments":"{}","status":"completed"}`
	client := newTestClient(t, Config{Model: ModelGPT6Luna}, serveEvents(
		ev("response.created", created),
		ev("response.output_item.done", `"output_index":0,"item":`+loadedCall),
		completed(loadedCall),
	))
	events := run(t, client)
	msg := *events[len(events)-1].Message
	msg.Provider, msg.API, msg.Model = providerName, API, ModelGPT6Luna

	history := []chat.Message{chat.NewUserTextMessage("hi"), msg,
		{Role: chat.RoleTool, ToolCallID: "call_9", ToolName: "report", Content: []chat.ContentBlock{chat.Text("ok")}}}
	input := paramsBody(t, mustNew(Config{Model: ModelGPT6Luna}), chat.Request{Messages: history, Tools: searchTools}).Input
	var call map[string]any
	if err := json.Unmarshal(input[1], &call); err != nil {
		t.Fatal(err)
	}
	if call["id"] != "fc_9" || call["namespace"] != "report" {
		t.Fatalf("the function call should be replayed with its id and namespace: %s", input[1])
	}
}
