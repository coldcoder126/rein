package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
)

type review struct {
	Score   int      `json:"score" jsonschema:"description=1 到 5 的评分"`
	Tags    []string `json:"tags,omitempty"`
	Comment struct {
		Text string `json:"text"`
	} `json:"comment"`
}

func TestSchemaForIsStrictAndStable(t *testing.T) {
	schema, err := chat.SchemaFor[review]()
	if err != nil {
		t.Fatal(err)
	}
	got := string(schema)
	for _, want := range []string{
		`"required":["comment","score","tags"]`, `"additionalProperties":false`,
		`"description":"1 到 5 的评分"`, `"required":["text"]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("schema missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, "$schema") || strings.Contains(got, "$ref") {
		t.Fatalf("schema should not contain $schema or refs: %s", got)
	}
	again, _ := chat.SchemaFor[review]()
	if string(again) != got {
		t.Fatal("the same type should always produce the same schema")
	}
}

func outputMsg(text string, reason chat.StopReason) *chat.Message {
	return &chat.Message{Role: chat.RoleAssistant, Content: []chat.ContentBlock{chat.Text(text)}, StopReason: reason}
}

func TestParseOutput(t *testing.T) {
	format := &chat.OutputFormat{Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`)}
	raw, err := chat.ParseOutput(outputMsg("```json\n{\"n\": 3}\n```", chat.StopReasonStop), format)
	if err != nil || string(raw) != `{"n": 3}` {
		t.Fatalf("should strip the code fence and validate: %q %v", raw, err)
	}
	if _, err := chat.ParseOutput(outputMsg(`[1,2]`, chat.StopReasonStop), nil); err != nil {
		t.Fatalf("without a schema only valid JSON is required: %v", err)
	}
	bad := map[string]*chat.Message{
		"not JSON":            outputMsg(`n = 3`, chat.StopReasonStop),
		"not matching schema": outputMsg(`{"n":"三"}`, chat.StopReasonStop),
		"extra field":         outputMsg(`{"n":3,"x":1}`, chat.StopReasonStop),
		"truncated":           outputMsg(`{"n":3}`, chat.StopReasonLength),
		"no text":             outputMsg(``, chat.StopReasonStop),
	}
	for name, msg := range bad {
		if _, err := chat.ParseOutput(msg, format); chat.AsError(err) == nil || chat.AsError(err).Kind != chat.KindInvalidOutput {
			t.Fatalf("%s should return invalid_output: %v", name, err)
		}
	}
}

func TestWithOutputInstructions(t *testing.T) {
	req := chat.WithOutputInstructions(chat.Request{
		SystemPrompt: "你是助手",
		Options:      chat.Options{Output: &chat.OutputFormat{Schema: json.RawMessage(`{"type":"object"}`)}},
	})
	if req.Output != nil || !strings.HasPrefix(req.SystemPrompt, "你是助手\n\n") || !strings.Contains(req.SystemPrompt, `{"type":"object"}`) {
		t.Fatalf("should write the schema into the system prompt and clear Output: %+v", req)
	}
	if plain := chat.WithOutputInstructions(chat.Request{SystemPrompt: "x"}); plain.SystemPrompt != "x" {
		t.Fatal("should not change the request without Output")
	}
}

func TestCompleteJSON(t *testing.T) {
	type answer struct {
		N int `json:"n"`
	}
	client := chattest.NewClient(chattest.Say(`{"n":7}`), chattest.Say(`{"n":"七"}`), chattest.Fail(errors.New("boom")))
	out, msg, err := chat.CompleteJSON[answer](context.Background(), client, chat.Request{})
	if err != nil || out.N != 7 || msg == nil {
		t.Fatalf("should parse the result: %+v %v", out, err)
	}
	if sent := client.Requests()[0].Output; sent == nil || !strings.Contains(string(sent.Schema), `"required":["n"]`) {
		t.Fatalf("should generate the schema from the type and send it: %+v", sent)
	}
	if _, msg, err := chat.CompleteJSON[answer](context.Background(), client, chat.Request{}); chat.AsError(err).Kind != chat.KindInvalidOutput || msg == nil {
		t.Fatalf("output not matching the schema should return invalid_output and keep the message: %v", err)
	}
	if _, _, err := chat.CompleteJSON[answer](context.Background(), client, chat.Request{}); err == nil || chat.AsError(err).Kind == chat.KindInvalidOutput {
		t.Fatalf("a failed model call should be returned as is: %v", err)
	}
}
