package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	goanthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
)

func sse(events ...[2]string) string {
	var sb strings.Builder
	for _, e := range events {
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", e[0], e[1])
	}
	return sb.String()
}

func ev(name, data string) [2]string { return [2]string{name, data} }

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return mustNew(Config{APIKey: "test", Endpoint: Endpoint{BaseURL: server.URL}, Model: sonnet46, MaxRetries: 0})
}

func serveSSE(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}
}

const messageStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-real","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":4}}}`

func TestStreamThinkingTextAndToolCalls(t *testing.T) {
	client := newTestClient(t, serveSSE(sse(
		ev("message_start", messageStart),
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想"}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig1"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"我来"}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"查"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":1}`),
		ev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"a\"}"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":2}`),
		ev("content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_2","name":"noargs","input":{}}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":3}`),
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`),
		ev("message_stop", `{"type":"message_stop"}`),
	)))

	events := chattest.Record(client.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	final := events[len(events)-1]
	if final.Type != chat.EventDone || final.Reason != chat.StopReasonToolUse {
		t.Fatalf("wrong terminal event: %+v", final)
	}
	msg := final.Message
	if len(msg.Content) != 4 {
		t.Fatalf("wrong number of content blocks: %+v", msg.Content)
	}
	if msg.Content[0].Type != chat.ContentTypeThinking || msg.Content[0].Text != "先想" || msg.Content[0].Signature != "sig1" {
		t.Fatalf("wrong thinking block: %+v", msg.Content[0])
	}
	if msg.Content[1].Text != "我来查" {
		t.Fatalf("wrong text block: %+v", msg.Content[1])
	}
	calls := msg.ToolCalls()
	if len(calls) != 2 || calls[0].ID != "toolu_1" || string(calls[0].Arguments) != `{"q":"a"}` || string(calls[1].Arguments) != `{}` {
		t.Fatalf("wrong tool calls: %+v", calls)
	}
	if msg.Provider != "anthropic" || msg.Model != sonnet46.Name || msg.ResponseID != "msg_1" || msg.ResponseModel != "claude-real" {
		t.Fatalf("wrong message metadata: %+v", msg)
	}
	if msg.Usage.Input != 10 || msg.Usage.Output != 42 || msg.Usage.CacheRead != 4 || msg.Usage.TotalTokens != 56 {
		t.Fatalf("wrong usage: %+v", msg.Usage)
	}
}

func TestStreamTruncatedIsRetryableFailureWithPartialContent(t *testing.T) {
	client := newTestClient(t, serveSSE(sse(
		ev("message_start", messageStart),
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"说到一半"}}`),
		ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`),
	)))

	events := chattest.Record(client.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	final := events[len(events)-1]
	if final.Type != chat.EventError || final.Reason != chat.StopReasonError {
		t.Fatalf("a dropped stream should end with error: %+v", final)
	}
	var ce *chat.Error
	if !errors.As(final.Err, &ce) || !ce.Retryable() {
		t.Fatalf("a dropped stream should be a retryable *chat.Error: %v", final.Err)
	}
	if len(final.Message.Content) != 1 || final.Message.Content[0].Text != "说到一半" {
		t.Fatalf("should keep generated text and drop the incomplete tool call: %+v", final.Message.Content)
	}
}

func TestStreamTruncatedToolArgsEndsAsLength(t *testing.T) {
	client := newTestClient(t, serveSSE(sse(
		ev("message_start", messageStart),
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"半"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":5}}`),
		ev("message_stop", `{"type":"message_stop"}`),
	)))
	events := chattest.Record(client.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	final := events[len(events)-1]
	calls := final.Message.ToolCalls()
	if final.Type != chat.EventDone || final.Reason != chat.StopReasonLength || len(calls) != 1 || string(calls[0].Arguments) != `{}` {
		t.Fatalf("truncated arguments should be cleared and end with length: %+v %+v", final, calls)
	}
}

func TestStreamRefusalBecomesError(t *testing.T) {
	refuse := serveSSE(sse(
		ev("message_start", messageStart),
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":1}}`),
		ev("message_stop", `{"type":"message_stop"}`),
	))
	stream := func(handler http.HandlerFunc) chat.Event {
		events := chattest.Record(newTestClient(t, handler).Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}))
		return events[len(events)-1]
	}

	final := stream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req_1")
		refuse(w, r)
	})
	if final.Type != chat.EventError || final.Message.ErrorKind != chat.KindRefusal {
		t.Fatalf("a refusal should be a non-retryable error: %+v", final)
	}
	if !strings.Contains(final.Message.ErrorMessage, "(request_id: req_1)") {
		t.Fatalf("a refusal should carry the request ID from the headers: %q", final.Message.ErrorMessage)
	}
	if final := stream(refuse); !strings.Contains(final.Message.ErrorMessage, "(request_id: msg_1)") {
		t.Fatalf("without the header a refusal should carry the message ID: %q", final.Message.ErrorMessage)
	}
}

func TestStreamHTTPErrorIsClassified(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	})
	events := chattest.Record(client.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	final := events[len(events)-1]
	if final.Type != chat.EventError || !errors.Is(final.Err, chat.ErrUnauthorized) {
		t.Fatalf("401 should be an auth failure: %+v", final)
	}
	if ce := chat.AsError(final.Err); ce.Status != 401 {
		t.Fatalf("should keep the status code: %+v", ce)
	}
}

func TestStreamCancellationIsAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse(ev("message_start", messageStart)))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	go func() {
		<-started
		cancel()
	}()

	events := chattest.Record(client.Stream(ctx, chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}))
	final := events[len(events)-1]
	if final.Type != chat.EventError || final.Reason != chat.StopReasonAborted || !errors.Is(final.Err, chat.ErrAborted) {
		t.Fatalf("cancellation should end with aborted: %+v", final)
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
