package gemini

import (
	"context"
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
	return mustNew(Config{Model: flash35, APIKey: "test", Endpoint: Endpoint{BaseURL: server.URL}})
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
