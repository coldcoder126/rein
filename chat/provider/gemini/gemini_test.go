package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
)

// serve starts a fake server that returns chunks as SSE and writes the received request body into body.
func serve(t *testing.T, body *string, status int, chunks ...string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body != nil {
			raw, _ := io.ReadAll(r.Body)
			*body = string(raw)
		}
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, chunks[0])
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	t.Cleanup(server.Close)
	return New(Config{APIKey: "test", BaseURL: server.URL})
}

func run(t *testing.T, client *Client, req chat.Request) chat.Event {
	t.Helper()
	if len(req.Messages) == 0 {
		req.Messages = []chat.Message{chat.NewUserTextMessage("hi")}
	}
	events := chattest.Record(client.Stream(context.Background(), req))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	return events[len(events)-1]
}

func TestStreamThinkingTextToolCallsAndImage(t *testing.T) {
	client := serve(t, nil, http.StatusOK,
		`{"responseId":"r1","modelVersion":"gemini-real","candidates":[{"content":{"role":"model","parts":[{"text":"先想","thought":true}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"我来"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"查"},{"thoughtSignature":"c2lnVA=="}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"lookup","args":{"q":"a"}},"thoughtSignature":"c2lnMQ=="},{"functionCall":{"name":"noargs"}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"aW1n"}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":5,"thoughtsTokenCount":7,"totalTokenCount":22}}`,
	)
	final := run(t, client, chat.Request{})
	if final.Type != chat.EventDone || final.Reason != chat.StopReasonToolUse {
		t.Fatalf("wrong terminal event: %+v %v", final, final.Err)
	}
	msg := final.Message
	if len(msg.Content) != 5 {
		t.Fatalf("wrong number of content blocks: %+v", msg.Content)
	}
	if msg.Content[0].Type != chat.ContentTypeThinking || msg.Content[0].Text != "先想" {
		t.Fatalf("wrong thinking block: %+v", msg.Content[0])
	}
	if msg.Content[1].Text != "我来查" || msg.Content[1].Signature != "c2lnVA==" {
		t.Fatalf("the text block should merge deltas and take the signature on the empty part: %+v", msg.Content[1])
	}
	calls := msg.ToolCalls()
	if len(calls) != 2 || calls[0].ID != "call_1" || string(calls[0].Arguments) != `{"q":"a"}` ||
		!strings.HasPrefix(calls[1].ID, generatedIDPrefix) || string(calls[1].Arguments) != `{}` {
		t.Fatalf("wrong tool calls: %+v", calls)
	}
	if msg.Content[2].Signature != "c2lnMQ==" || msg.Content[3].Signature != "" {
		t.Fatalf("signatures should stay on their original function calls: %+v", msg.Content[2:4])
	}
	if msg.Content[4].Type != chat.ContentTypeMedia || string(msg.Content[4].Data) != "img" {
		t.Fatalf("wrong image block: %+v", msg.Content[4])
	}
	if msg.ResponseID != "r1" || msg.ResponseModel != "gemini-real" {
		t.Fatalf("wrong message metadata: %+v", msg)
	}
	if u := msg.Usage; u.Input != 6 || u.CacheRead != 4 || u.Output != 12 || u.TotalTokens != 22 {
		t.Fatalf("wrong usage: %+v", u)
	}
}

func TestStreamTerminalStates(t *testing.T) {
	text := `{"candidates":[{"content":{"role":"model","parts":[{"text":"半截"}]}}]}`
	cases := []struct {
		name   string
		chunks []string
		reason chat.StopReason
		kind   chat.ErrorKind
	}{
		{"truncated", []string{text, `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`}, chat.StopReasonLength, ""},
		{"dropped stream", []string{text}, chat.StopReasonError, chat.KindNetwork},
		{"safety block", []string{text, `{"candidates":[{"finishReason":"SAFETY"}]}`}, chat.StopReasonError, chat.KindRefusal},
		{"prompt blocked", []string{`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`}, chat.StopReasonError, chat.KindRefusal},
		{"malformed function call", []string{`{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}`}, chat.StopReasonError, chat.KindServer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			final := run(t, serve(t, nil, http.StatusOK, tc.chunks...), chat.Request{})
			if final.Reason != tc.reason || final.Message.ErrorKind != tc.kind {
				t.Fatalf("wrong terminal state: %+v err=%v", final, final.Err)
			}
		})
	}
}

func TestStreamHTTPErrorIsClassified(t *testing.T) {
	client := serve(t, nil, http.StatusTooManyRequests, `{"error":{"code":429,"message":"Resource exhausted","status":"RESOURCE_EXHAUSTED"}}`)
	final := run(t, client, chat.Request{})
	ce := chat.AsError(final.Err)
	if final.Type != chat.EventError || !errors.Is(final.Err, chat.ErrRateLimited) || ce.Status != 429 || !ce.Retryable() {
		t.Fatalf("429 should be a retryable rate limit: %+v", ce)
	}
}

func TestRequestBody(t *testing.T) {
	var body string
	client := serve(t, &body, http.StatusOK, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	call := func(id, sig string) chat.ContentBlock {
		return chat.ContentBlock{Type: chat.ContentTypeToolCall, Signature: sig,
			ToolCall: &chat.ToolCall{ID: id, Name: "lookup", Arguments: json.RawMessage(`{"q":"a"}`)}}
	}
	run(t, client, chat.Request{
		SystemPrompt: "系统提示",
		Options:      chat.Options{MaxTokens: 100, Temperature: chat.Float(0), Reasoning: chat.ReasoningMedium, Output: &chat.OutputFormat{Schema: json.RawMessage(`{"type":"object"}`)}},
		Tools:        []chat.ToolDef{{Name: "lookup", Description: "查", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"additionalProperties":false}`)}},
		Messages: []chat.Message{
			{Role: chat.RoleUser, Content: []chat.ContentBlock{chat.Text("看图"), {Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: []byte("img")},
				chat.MediaURL("https://example.com/clip.mp4"), chat.Media([]byte("PK\x03\x04")).WithName("表格.xlsx")}},
			{Role: chat.RoleAssistant, Provider: providerName, Model: defaultModel, Content: []chat.ContentBlock{
				{Type: chat.ContentTypeThinking, Text: "无签名的思考"},
				call("call_1", "c2lnMQ=="), call(generatedIDPrefix+"1", ""),
			}},
			{Role: chat.RoleTool, ToolCallID: "call_1", ToolName: "lookup", Content: []chat.ContentBlock{chat.Text("r1")}},
			{Role: chat.RoleTool, ToolCallID: generatedIDPrefix + "1", ToolName: "lookup", IsError: true,
				Content: []chat.ContentBlock{{Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: []byte("img")}}},
			{Role: chat.RoleAssistant, Provider: "openai", Model: "gpt", Content: []chat.ContentBlock{call("call_x", "")}},
			{Role: chat.RoleTool, ToolCallID: "call_x", ToolName: "lookup", Content: []chat.ContentBlock{chat.Text("rx")}},
		},
	})
	for _, want := range []string{
		`"systemInstruction":{"parts":[{"text":"系统提示"}]`,
		`"maxOutputTokens":100`, `"temperature":0`, `"responseMimeType":"application/json"`, `"responseJsonSchema":{"type":"object"}`,
		`"thinkingLevel":"MEDIUM"`, `"includeThoughts":true`,
		`"parametersJsonSchema":{"additionalProperties":false`,
		`"inlineData":{"data":"aW1n","mimeType":"image/png"}`,
		`"fileData":{"fileUri":"https://example.com/clip.mp4","mimeType":"video/mp4"}`,
		`"inlineData":{"data":"UEsDBA==","mimeType":"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}`,
		`"functionCall":{"args":{"q":"a"},"id":"call_1","name":"lookup"},"thoughtSignature":"c2lnMQ=="`,
		`{"functionCall":{"args":{"q":"a"},"name":"lookup"}}`,
		`"functionResponse":{"id":"call_1","name":"lookup","response":{"output":"r1"}}`,
		`"response":{"error":"(see attached content)"}`,
		`"thoughtSignature":"c2tpcF90aG91Z2h0X3NpZ25hdHVyZV92YWxpZGF0b3I="`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("request body missing %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "无签名的思考") || strings.Contains(body, generatedIDPrefix) {
		t.Fatalf("unsigned thinking and locally generated IDs should not be sent:\n%s", body)
	}
	var parsed struct {
		Contents []struct {
			Role  string            `json:"role"`
			Parts []json.RawMessage `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Contents) != 5 || parsed.Contents[1].Role != "model" || len(parsed.Contents[2].Parts) != 2 {
		t.Fatalf("consecutive tool results should merge into one user content: %s", body)
	}
}

func TestThinkingConfigByModel(t *testing.T) {
	cases := []struct {
		model string
		level chat.ReasoningLevel
		want  string
	}{
		{ModelGemini3_5Flash, "", `{"includeThoughts":true}`},
		{ModelGemini3_5Flash, chat.ReasoningOff, `{"thinkingBudget":0}`},
		{ModelGemini3_5Flash, chat.ReasoningXHigh, `{"includeThoughts":true,"thinkingLevel":"HIGH"}`},
		{ModelGemini2_5Flash, chat.ReasoningOff, `{"thinkingBudget":0}`},
		{ModelGemini2_5Flash, chat.ReasoningLow, `{"includeThoughts":true,"thinkingBudget":2048}`},
	}
	for _, tc := range cases {
		cfg, err := thinkingConfig(tc.model, tc.level)
		if err != nil {
			t.Fatal(err)
		}
		if raw, _ := json.Marshal(cfg); string(raw) != tc.want {
			t.Fatalf("%s/%s: %s", tc.model, tc.level, raw)
		}
	}
	if _, err := thinkingConfig(ModelGemini3_5Flash, "extreme"); err == nil {
		t.Fatal("an unknown reasoning level should fail")
	}
	// 2.x image models do not support thinking: no thinking config by default, and image output is requested.
	_, config, err := New(Config{Model: "gemini-2.5-flash-image", ImageGeneration: true}).buildRequest(chat.Request{})
	if err != nil || config.ThinkingConfig != nil || len(config.ResponseModalities) != 2 {
		t.Fatalf("wrong image model config: %+v %v", config, err)
	}
}

func TestToolChoiceConfig(t *testing.T) {
	tools := []chat.ToolDef{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	for choice, want := range map[chat.ToolChoice]string{
		{Mode: chat.ToolChoiceRequired}: `{"functionCallingConfig":{"mode":"ANY"}}`,
		{Mode: chat.ToolChoiceNone}:     `{"functionCallingConfig":{"mode":"NONE"}}`,
		chat.ForceTool("lookup"):        `{"functionCallingConfig":{"allowedFunctionNames":["lookup"],"mode":"ANY"}}`,
	} {
		_, config, err := New(Config{}).buildRequest(chat.Request{Options: chat.Options{ToolChoice: choice}, Tools: tools})
		if err != nil {
			t.Fatal(err)
		}
		if raw, _ := json.Marshal(config.ToolConfig); string(raw) != want {
			t.Fatalf("wrong ToolConfig: %s", raw)
		}
	}
	_, config, _ := New(Config{}).buildRequest(chat.Request{Tools: tools})
	if config.ToolConfig != nil {
		t.Fatal("ToolConfig should not be sent without ToolChoice")
	}
}
