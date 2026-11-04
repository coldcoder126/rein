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
	return convertMessages(messages, false)
}

func toolCallBlock(id, name, args string) chat.ContentBlock {
	return chat.ContentBlock{Type: chat.ContentTypeToolCall, ToolCall: &chat.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}}
}

func TestBuildParamsAddsSystemPromptAndTools(t *testing.T) {
	client := New(Config{Model: ModelClaudeHaiku4_5})
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
	client := New(Config{Model: ModelClaudeSonnet5_5})
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
	}})
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
			Provider: providerName,
			Model:    ModelClaudeSonnet4_6,
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
	client := New(Config{Model: ModelClaudeHaiku4_5})
	failed := chat.Message{
		Role: chat.RoleAssistant, Provider: providerName, Model: ModelClaudeHaiku4_5,
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

func TestStopReasonFromAnthropic(t *testing.T) {
	cases := map[goanthropic.StopReason]chat.StopReason{
		goanthropic.StopReasonToolUse:                    chat.StopReasonToolUse,
		goanthropic.StopReasonMaxTokens:                  chat.StopReasonLength,
		goanthropic.StopReasonEndTurn:                    chat.StopReasonStop,
		goanthropic.StopReasonRefusal:                    chat.StopReasonError,
		goanthropic.StopReasonPauseTurn:                  chat.StopReasonStop,
		goanthropic.StopReasonModelContextWindowExceeded: chat.StopReasonLength,
	}
	for in, want := range cases {
		if got := stopReasonFromAnthropic(in); got != want {
			t.Fatalf("%s mapped wrongly: %q", in, got)
		}
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
	client := New(Config{Model: ModelClaudeSonnet5_5})
	params, err := client.buildParams(chat.Request{Messages: []chat.Message{
		{Role: chat.RoleUser, Content: []chat.ContentBlock{
			chat.MediaURL("https://example.com/a.webp"),
			chat.Media([]byte("%PDF-1.7 正文")).WithName("合同.pdf"),
			chat.MediaURL("https://example.com/b.pdf"),
			chat.Media([]byte("a,b\n1,2")).WithName("数据.csv"),
		}},
		{Role: chat.RoleAssistant, Provider: providerName, Model: client.cfg.Model, Content: []chat.ContentBlock{toolCallBlock("c1", "export", `{}`)}},
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

func TestInfoReportsImageSupport(t *testing.T) {
	info := New(Config{Model: ModelClaudeSonnet5_5}).Info()
	if !info.SupportsMedia || info.ImageLimits.MaxBytes == 0 {
		t.Fatalf("should support media by default and give image specs: %+v", info)
	}
	if New(Config{Model: ModelClaudeSonnet5_5, TextOnly: true}).Info().SupportsMedia {
		t.Fatal("TextOnly should turn media support off")
	}
}

func TestBuildParamsReplacesImagesForTextOnlyEndpoint(t *testing.T) {
	client := New(Config{Model: ModelClaudeSonnet5_5, TextOnly: true})
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

func TestBuildParamsReasoning(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		opts    chat.Options
		want    []string
		notWant []string
	}{
		{"no thinking parameters by default", Config{Model: ModelClaudeSonnet5_5}, chat.Options{}, nil, []string{`"thinking"`, `"output_config"`}},
		{"adaptive models use effort", Config{Model: ModelClaudeSonnet5_5}, chat.Options{Reasoning: chat.ReasoningXHigh, Temperature: chat.Float(0.2)},
			[]string{`"thinking":{"display":"summarized","type":"adaptive"}`, `"effort":"xhigh"`}, []string{`"temperature"`}},
		{"fall back to high without xhigh", Config{Model: ModelClaudeSonnet4_6}, chat.Options{Reasoning: chat.ReasoningXHigh},
			[]string{`"type":"adaptive"`, `"effort":"high"`}, nil},
		{"models that cannot turn thinking off use the lowest effort", Config{Model: ModelClaudeSonnet5_5}, chat.Options{Reasoning: chat.ReasoningOff},
			[]string{`"effort":"low"`}, []string{`"thinking"`}},
		{"models that can turn it off send disabled", Config{Model: ModelClaudeHaiku5_5}, chat.Options{Reasoning: chat.ReasoningOff, Temperature: chat.Float(0.2)},
			[]string{`"thinking":{"type":"disabled"}`, `"temperature":0.2`}, []string{`"output_config"`}},
		{"older models use a budget and raise max_tokens", Config{Model: ModelClaudeHaiku4_5}, chat.Options{Reasoning: chat.ReasoningMedium, MaxTokens: 1000},
			[]string{`"budget_tokens":8192`, `"type":"enabled"`, `"max_tokens":9192`}, []string{`"output_config"`}},
		{"budget within the model output limit", Config{Model: ModelClaudeHaiku4_5, MaxOutputTokens: 4096}, chat.Options{Reasoning: chat.ReasoningHigh},
			[]string{`"budget_tokens":3072`, `"max_tokens":4096`}, nil},
		{"compatible endpoints force budget thinking", Config{Model: "deepseek-v4-flash"}, chat.Options{Reasoning: chat.ReasoningLow},
			[]string{`"budget_tokens":2048`}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := paramsJSON(t, New(tc.cfg), chat.Request{Options: tc.opts})
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %s: %s", want, got)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Fatalf("should not contain %s: %s", notWant, got)
				}
			}
		})
	}
	if _, err := New(Config{Model: ModelClaudeSonnet5_5}).buildParams(chat.Request{Options: chat.Options{Reasoning: "extreme"}}); err == nil {
		t.Fatal("an unknown reasoning level should fail")
	}
}

func TestBuildParamsCacheBreakpoints(t *testing.T) {
	client := New(Config{Model: ModelClaudeSonnet5_5})
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

func TestConvertUnsignedThinking(t *testing.T) {
	msgs := []chat.Message{{Role: chat.RoleAssistant, Content: []chat.ContentBlock{
		{Type: chat.ContentTypeThinking, Text: "没签名的思考"},
		{Type: chat.ContentTypeThinking},
		chat.Text("回答"),
	}}}
	out, err := convertMessages(msgs, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), `"type":"thinking"`) || !strings.Contains(string(raw), `"text":"没签名的思考"`) {
		t.Fatalf("unsigned thinking should become text and empty blocks be dropped: %s", raw)
	}
	out, _ = convertMessages(msgs[:1], true)
	raw, _ = json.Marshal(out)
	if !strings.Contains(string(raw), `"thinking":"没签名的思考"`) {
		t.Fatalf("UnsignedThinking should replay as is: %s", raw)
	}
}

func TestInfoUsesModelSpec(t *testing.T) {
	cases := []struct {
		cfg        Config
		input, out int
	}{
		{Config{Model: ModelClaudeSonnet5_5}, 1_000_000, 128_000},
		{Config{Model: ModelClaudeHaiku4_5}, 200_000, 64_000},
		{Config{Model: "other-model"}, 200_000, 8_192},
		{Config{Model: ModelClaudeSonnet5_5, MaxInputTokens: 50_000, MaxOutputTokens: 2_000}, 50_000, 2_000},
	}
	for _, tc := range cases {
		info := New(tc.cfg).Info()
		if info.MaxInputTokens != tc.input || info.MaxOutputTokens != tc.out {
			t.Fatalf("%q has the wrong spec: %+v", tc.cfg.Model, info)
		}
	}
}

func TestBuildParamsStructuredOutput(t *testing.T) {
	output := chat.Options{Reasoning: chat.ReasoningLow, Output: &chat.OutputFormat{Schema: json.RawMessage(`{"type":"object"}`)}}
	got := paramsJSON(t, New(Config{Model: ModelClaudeSonnet5_5}), chat.Request{Options: output})
	if !strings.Contains(got, `"output_config":{"effort":"low","format":{"schema":{"type":"object"},"type":"json_schema"}}`) {
		t.Fatalf("should use native output_config.format alongside effort: %s", got)
	}
	got = paramsJSON(t, New(Config{Model: "deepseek-v4-flash"}), chat.Request{SystemPrompt: "你是助手", Options: output})
	if strings.Contains(got, "json_schema") || !strings.Contains(got, `JSON Schema:\n\n{\"type\":\"object\"}`) {
		t.Fatalf("compatible endpoints should write the schema into the system prompt: %s", got)
	}
	if New(Config{Model: "deepseek-v4-flash"}).Info().StructuredOutput || !New(Config{Model: ModelClaudeSonnet5_5}).Info().StructuredOutput {
		t.Fatal("model info should reflect native structured output support")
	}
}

func TestBuildParamsToolChoice(t *testing.T) {
	tools := []chat.ToolDef{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	req := func(choice chat.ToolChoice, level chat.ReasoningLevel) chat.Request {
		return chat.Request{Options: chat.Options{ToolChoice: choice, Reasoning: level}, Tools: tools}
	}
	required := chat.ToolChoice{Mode: chat.ToolChoiceRequired}
	if got := paramsJSON(t, New(Config{Model: ModelClaudeHaiku5_5}), req(chat.ForceTool("lookup"), chat.ReasoningHigh)); !strings.Contains(got, `"tool_choice":{"name":"lookup","type":"tool"}`) || !strings.Contains(got, `"type":"adaptive"`) {
		t.Fatalf("adaptive thinking models should keep both thinking and the forced tool: %s", got)
	}
	if got := paramsJSON(t, New(Config{Model: ModelClaudeHaiku4_5}), req(required, chat.ReasoningHigh)); !strings.Contains(got, `"tool_choice":{"type":"any"}`) || strings.Contains(got, `"thinking"`) {
		t.Fatalf("budget thinking models should not enable thinking with forced tool calls: %s", got)
	}
	if got := paramsJSON(t, New(Config{Model: ModelClaudeSonnet5_5}), req(chat.ToolChoice{Mode: chat.ToolChoiceNone}, "")); !strings.Contains(got, `"tool_choice":{"type":"none"}`) {
		t.Fatalf("should send none: %s", got)
	}
	r := req(required, "")
	r.Messages = []chat.Message{chat.NewUserTextMessage("hi")}
	if _, err := New(Config{Model: ModelClaudeSonnet5_5}).buildParams(r); err == nil || !strings.Contains(err.Error(), "does not support forced tool calls") {
		t.Fatalf("models that cannot turn thinking off should reject forced tool calls: %v", err)
	}
	r.Tools = nil
	if _, err := New(Config{Model: ModelClaudeSonnet5_5}).buildParams(r); err != nil {
		t.Fatalf("ToolChoice should be ignored without tools: %v", err)
	}
}
