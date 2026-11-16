package anthropic

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	goanthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/coldcoder126/rein/chat"
)

func convertMessagesT(messages []chat.Message) ([]goanthropic.MessageParam, error) {
	return convertMessages(messages, false, false)
}

func toolCallBlock(id, name, args string) chat.ContentBlock {
	return chat.ContentBlock{Type: chat.ContentTypeToolCall, ToolCall: &chat.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}}
}

func TestBuildParamsAddsSystemPromptAndTools(t *testing.T) {
	client := mustNew(Config{Model: haiku45})
	params, err := client.buildParams(chat.Request{
		SystemPrompt: "系统提示",
		Messages:     []chat.Message{chat.NewUserTextMessage("你好")},
		Tools: []chat.ToolDef{{
			Name:        "search",
			Description: "搜索",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(params.System) != 1 || params.System[0].Text != "系统提示" {
		t.Fatalf("wrong system prompt conversion: %+v", params.System)
	}
	if len(params.Messages) != 1 || len(params.Tools) != 1 {
		t.Fatalf("wrong number of messages or tools: %d %d", len(params.Messages), len(params.Tools))
	}
}

func TestBuildParamsTemperatureZeroIsSent(t *testing.T) {
	client := mustNew(Config{Model: sonnet55})
	params, err := client.buildParams(chat.Request{
		Options:  chat.Options{Temperature: chat.Float(0)},
		Messages: []chat.Message{chat.NewUserTextMessage("你好")},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(params)
	if !strings.Contains(string(raw), `"temperature":0`) {
		t.Fatalf("an explicit temperature=0 should be sent: %s", raw)
	}
	params, _ = client.buildParams(chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("你好")}})
	raw, _ = json.Marshal(params)
	if strings.Contains(string(raw), `"temperature"`) {
		t.Fatalf("temperature should not be sent when unset: %s", raw)
	}
}

func TestConvertToolsKeepsFullSchema(t *testing.T) {
	tools, err := convertTools([]chat.ToolDef{{
		Name: "plan",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"step":{"$ref":"#/$defs/step"}},
			"required":["step"],
			"additionalProperties":false,
			"$defs":{"step":{"type":"string","enum":["a","b"]}}
		}`),
	}}, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tools[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"$defs"`, `"additionalProperties":false`, `"$ref":"#/$defs/step"`, `"required":["step"]`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("schema lost %s: %s", want, raw)
		}
	}
}

func TestConvertAssistantKeepsBlockOrder(t *testing.T) {
	msgs, err := convertMessagesT([]chat.Message{
		chat.NewUserTextMessage("查"),
		{
			Role:     chat.RoleAssistant,
			Provider: providerName, API: API,
			Model: sonnet46.Name,
			Content: []chat.ContentBlock{
				{Type: chat.ContentTypeThinking, Text: "先想", Signature: "sig1"},
				chat.Text("我来查"),
				toolCallBlock("call_1", "lookup", `{"q":"a"}`),
				{Type: chat.ContentTypeThinking, Text: "再想", Signature: "sig2"},
				toolCallBlock("call_2", "lookup", `{"q":"b"}`),
			},
		},
		{Role: chat.RoleTool, ToolCallID: "call_1", Content: []chat.ContentBlock{chat.Text("r1")}},
		{Role: chat.RoleTool, ToolCallID: "call_2", IsError: true, Content: []chat.ContentBlock{chat.Text("r2")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("consecutive tool results should merge into one user message: %d", len(msgs))
	}
	raw, _ := json.Marshal(msgs[1])
	got := string(raw)
	order := []string{`"thinking":"先想"`, `"text":"我来查"`, `"id":"call_1"`, `"thinking":"再想"`, `"id":"call_2"`}
	last := -1
	for _, want := range order {
		idx := strings.Index(got, want)
		if idx <= last {
			t.Fatalf("block order changed, %s at %d is not after %d: %s", want, idx, last, got)
		}
		last = idx
	}
	toolMsg, _ := json.Marshal(msgs[2])
	if strings.Count(string(toolMsg), `"type":"tool_result"`) != 2 || !strings.Contains(string(toolMsg), `"is_error":true`) {
		t.Fatalf("wrong tool result conversion: %s", toolMsg)
	}
}

func TestBuildParamsNormalizesReplay(t *testing.T) {
	client := mustNew(Config{Model: haiku45})
	failed := chat.Message{
		Role: chat.RoleAssistant, Provider: providerName, API: API, Model: haiku45.Name,
		Content: []chat.ContentBlock{chat.Text("半截")}, StopReason: chat.StopReasonError,
	}
	foreign := chat.Message{
		Role: chat.RoleAssistant, Provider: "openai", Model: "gpt",
		Content:    []chat.ContentBlock{toolCallBlock("call|1", "lookup", `{}`)},
		StopReason: chat.StopReasonToolUse,
	}
	params, err := client.buildParams(chat.Request{Messages: []chat.Message{
		chat.NewUserTextMessage("一"), failed, chat.NewUserTextMessage("二"), foreign,
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(params.Messages)
	got := string(raw)
	if strings.Contains(got, "半截") {
		t.Fatalf("failed assistant messages should not be replayed: %s", got)
	}
	if !strings.Contains(got, `"id":"call_1"`) || !strings.Contains(got, `"tool_use_id":"call_1"`) {
		t.Fatalf("tool call IDs across models should be normalized and missing results filled: %s", got)
	}
}

func TestNormalizeToolCallID(t *testing.T) {
	if got := normalizeToolCallID("call|abc.123"); got != "call_abc_123" {
		t.Fatalf("invalid characters should be replaced: %s", got)
	}
	if got := normalizeToolCallID(strings.Repeat("a", 100)); len(got) != 64 {
		t.Fatalf("should truncate to 64: %d", len(got))
	}
}

func imageBlock(mime string) chat.ContentBlock {
	return chat.ContentBlock{Type: chat.ContentTypeMedia, MIMEType: mime, Data: []byte("img")}
}

func TestConvertUserMessageWithImages(t *testing.T) {
	msgs, err := convertMessagesT([]chat.Message{{
		Role:    chat.RoleUser,
		Content: []chat.ContentBlock{chat.Text("看这张图"), imageBlock("image/png"), chat.Text("")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(msgs)
	got := string(raw)
	if !strings.Contains(got, `"type":"image"`) || !strings.Contains(got, `"media_type":"image/png"`) ||
		!strings.Contains(got, `"data":"aW1n"`) || strings.Count(got, `"type":"text"`) != 1 {
		t.Fatalf("wrong user image conversion: %s", got)
	}
	if strings.Index(got, `"type":"text"`) > strings.Index(got, `"type":"image"`) {
		t.Fatalf("should keep the original order of text before image: %s", got)
	}
}

func TestConvertToolResultWithImages(t *testing.T) {
	msgs, err := convertMessagesT([]chat.Message{
		{Role: chat.RoleTool, ToolCallID: "c1", Content: []chat.ContentBlock{chat.Text("截图如下"), imageBlock("image/jpeg")}},
		{Role: chat.RoleTool, ToolCallID: "c2", Content: []chat.ContentBlock{imageBlock("image/png")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("consecutive tool results should merge into one user message: %d", len(msgs))
	}
	raw, _ := json.Marshal(msgs[0])
	got := string(raw)
	if strings.Count(got, `"type":"tool_result"`) != 2 || strings.Count(got, `"type":"image"`) != 2 {
		t.Fatalf("wrong tool result image conversion: %s", got)
	}
	if !strings.Contains(got, "截图如下") || !strings.Contains(got, "(see attached content)") {
		t.Fatalf("an image-only tool result should get placeholder text, and text should be kept when present: %s", got)
	}
}

func TestConvertMediaKinds(t *testing.T) {
	client := mustNew(Config{Model: sonnet55})
	params, err := client.buildParams(chat.Request{Messages: []chat.Message{
		{Role: chat.RoleUser, Content: []chat.ContentBlock{
			chat.MediaURL("https://example.com/a.webp"),
			chat.Media([]byte("%PDF-1.7 正文")).WithName("合同.pdf"),
			chat.MediaURL("https://example.com/b.pdf"),
			chat.Media([]byte("a,b\n1,2")).WithName("数据.csv"),
		}},
		{Role: chat.RoleAssistant, Provider: providerName, API: API, Model: client.cfg.Model.Name, Content: []chat.ContentBlock{toolCallBlock("c1", "export", `{}`)}},
		{Role: chat.RoleTool, ToolCallID: "c1", Content: []chat.ContentBlock{chat.Media([]byte("%PDF-1.7 报表")).WithName("报表.pdf")}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(params.Messages)
	got := string(raw)
	for _, want := range []string{
		`"source":{"url":"https://example.com/a.webp","type":"url"},"type":"image"`,
		`"media_type":"application/pdf","type":"base64"},"title":"合同.pdf"`,
		`"source":{"url":"https://example.com/b.pdf","type":"url"}`,
		`"source":{"data":"a,b\n1,2","media_type":"text/plain","type":"text"},"title":"数据.csv"`,
		`"title":"报表.pdf","type":"document"`,
		`(see attached content)`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s:\n%s", want, got)
		}
	}
	if strings.Count(got, `"type":"document"`) != 4 {
		t.Fatalf("should have four document blocks (two PDFs, one text, one in the tool result): %s", got)
	}

	// Media the Messages API has no input for: neither replaced nor dropped, but a clear error for the caller to handle.
	for name, block := range map[string]chat.ContentBlock{
		"音频":          chat.Media([]byte("ID3\x03\x00\x00\x00\x00\x00\x00")).WithName("录音.mp3"),
		"text by URL": chat.MediaURL("https://example.com/notes.md"),
		"未知类型":        chat.Media([]byte{0, 1, 2, 3}),
	} {
		_, err := client.buildParams(chat.Request{Messages: []chat.Message{{Role: chat.RoleUser, Content: []chat.ContentBlock{block}}}})
		var ce *chat.Error
		if !errors.As(err, &ce) || ce.Kind != chat.KindInvalidRequest || !strings.Contains(ce.Message, block.MIMEType) {
			t.Fatalf("%s: should return an invalid_request error naming the type: %v", name, err)
		}
	}
}

func TestBuildParamsReplacesImagesForTextOnlyEndpoint(t *testing.T) {
	client := mustNew(Config{Model: sonnet55, Endpoint: Endpoint{Compat: Compat{NoMedia: true}}})
	params, err := client.buildParams(chat.Request{Messages: []chat.Message{{
		Role:    chat.RoleUser,
		Content: []chat.ContentBlock{chat.Text("看图"), imageBlock("image/png")},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(params.Messages)
	if strings.Contains(string(raw), `"type":"image"`) || !strings.Contains(string(raw), "attachment omitted") {
		t.Fatalf("a text endpoint should replace images with placeholder text: %s", raw)
	}
}

func paramsJSON(t *testing.T, client *Client, req chat.Request) string {
	t.Helper()
	if len(req.Messages) == 0 {
		req.Messages = []chat.Message{chat.NewUserTextMessage("你好")}
	}
	params, err := client.buildParams(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestConvertUnsignedThinking(t *testing.T) {
	msgs := []chat.Message{{Role: chat.RoleAssistant, Content: []chat.ContentBlock{
		{Type: chat.ContentTypeThinking, Text: "没签名的思考"},
		{Type: chat.ContentTypeThinking},
		chat.Text("回答"),
	}}}
	out, err := convertMessages(msgs, false, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"type":"thinking"`) || !strings.Contains(string(raw), `"text":"没签名的思考"`) {
		t.Fatalf("unsigned thinking should become text and empty blocks be dropped: %s", raw)
	}
	out, _ = convertMessages(msgs[:1], true, false)
	raw, _ = json.Marshal(out)
	if !strings.Contains(string(raw), `"thinking":"没签名的思考"`) {
		t.Fatalf("UnsignedThinking should replay as is: %s", raw)
	}
}

func TestBuildParamsStructuredOutput(t *testing.T) {
	output := chat.Options{Reasoning: chat.ReasoningLow, Output: &chat.OutputFormat{Schema: json.RawMessage(`{"type":"object"}`)}}
	got := paramsJSON(t, mustNew(Config{Model: sonnet55}), chat.Request{Options: output})
	if !strings.Contains(got, `"output_config":{"effort":"low","format":{"schema":{"type":"object"},"type":"json_schema"}}`) {
		t.Fatalf("should use native output_config.format alongside effort: %s", got)
	}
	got = paramsJSON(t, mustNew(Config{Model: thirdParty}), chat.Request{SystemPrompt: "你是助手", Options: output})
	if strings.Contains(got, "json_schema") || !strings.Contains(got, `JSON Schema:\n\n{\"type\":\"object\"}`) {
		t.Fatalf("compatible endpoints should write the schema into the system prompt: %s", got)
	}
	if mustNew(Config{Model: thirdParty}).Info().StructuredOutput || !mustNew(Config{Model: sonnet55}).Info().StructuredOutput {
		t.Fatal("model info should reflect native structured output support")
	}
}

func TestBuildParamsToolChoice(t *testing.T) {
	tools := []chat.ToolDef{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	req := func(choice chat.ToolChoice, level chat.ReasoningLevel) chat.Request {
		return chat.Request{Options: chat.Options{ToolChoice: choice, Reasoning: level}, Tools: tools}
	}
	required := chat.ToolChoice{Mode: chat.ToolChoiceRequired}
	if got := paramsJSON(t, mustNew(Config{Model: haiku55}), req(chat.ForceTool("lookup"), chat.ReasoningHigh)); !strings.Contains(got, `"tool_choice":{"name":"lookup","type":"tool"}`) || !strings.Contains(got, `"type":"adaptive"`) {
		t.Fatalf("adaptive thinking models should keep both thinking and the forced tool: %s", got)
	}
	if got := paramsJSON(t, mustNew(Config{Model: haiku45}), req(required, chat.ReasoningHigh)); !strings.Contains(got, `"tool_choice":{"type":"any"}`) || strings.Contains(got, `"thinking"`) {
		t.Fatalf("budget thinking models should not enable thinking with forced tool calls: %s", got)
	}
	if got := paramsJSON(t, mustNew(Config{Model: sonnet55}), req(chat.ToolChoice{Mode: chat.ToolChoiceNone}, "")); !strings.Contains(got, `"tool_choice":{"type":"none"}`) {
		t.Fatalf("should send none: %s", got)
	}
	r := req(required, "")
	r.Messages = []chat.Message{chat.NewUserTextMessage("hi")}
	if _, err := mustNew(Config{Model: sonnet55}).buildParams(r); err == nil || !strings.Contains(err.Error(), "does not support forced tool calls") {
		t.Fatalf("models that cannot turn thinking off should reject forced tool calls: %v", err)
	}
	r.Tools = nil
	if _, err := mustNew(Config{Model: sonnet55}).buildParams(r); err != nil {
		t.Fatalf("ToolChoice should be ignored without tools: %v", err)
	}
}
