package gemini

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

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
			{Role: chat.RoleAssistant, Provider: providerName, API: API, Model: flash35.Name, Content: []chat.ContentBlock{
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

func TestToolChoiceConfig(t *testing.T) {
	tools := []chat.ToolDef{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	for choice, want := range map[chat.ToolChoice]string{
		{Mode: chat.ToolChoiceRequired}: `{"functionCallingConfig":{"mode":"ANY"}}`,
		{Mode: chat.ToolChoiceNone}:     `{"functionCallingConfig":{"mode":"NONE"}}`,
		chat.ForceTool("lookup"):        `{"functionCallingConfig":{"allowedFunctionNames":["lookup"],"mode":"ANY"}}`,
	} {
		_, config, err := mustNew(Config{Model: flash35}).buildRequest(chat.Request{Options: chat.Options{ToolChoice: choice}, Tools: tools})
		if err != nil {
			t.Fatal(err)
		}
		if raw, _ := json.Marshal(config.ToolConfig); string(raw) != want {
			t.Fatalf("wrong ToolConfig: %s", raw)
		}
	}
	_, config, _ := mustNew(Config{Model: flash35}).buildRequest(chat.Request{Tools: tools})
	if config.ToolConfig != nil {
		t.Fatal("ToolConfig should not be sent without ToolChoice")
	}
}
