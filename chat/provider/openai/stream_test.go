package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
)

func newTestClient(t *testing.T, cfg Config, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg.APIKey, cfg.BaseURL = "test", server.URL+"/"
	return New(cfg)
}

type sseEvent struct{ typ, data string }

func serveEvents(events ...sseEvent) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var sb strings.Builder
		for _, e := range events {
			fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", e.typ, e.data)
		}
		fmt.Fprint(w, sb.String())
	}
}

func ev(typ, body string) sseEvent {
	return sseEvent{typ, fmt.Sprintf(`{"type":"%s",%s}`, typ, body)}
}

func run(t *testing.T, c *Client) []chat.Event {
	t.Helper()
	events := chattest.Record(c.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	return events
}

const (
	created      = `"response":{"id":"resp_1","model":"gpt-real","status":"in_progress","output":[]}`
	reasoningRaw = `{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"先想"},{"type":"summary_text","text":"二"}],"encrypted_content":"ENC"}`
	messageRaw   = `{"id":"msg_1","type":"message","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"我来查","annotations":[]}]}`
	callRaw      = `{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"a\"}","status":"completed"}`
	call2Raw     = `{"id":"fc_2","type":"function_call","call_id":"call_2","name":"noargs","arguments":"","status":"completed"}`
	imageRaw     = `{"id":"ig_1","type":"image_generation_call","status":"completed","result":"aW1n","output_format":"png"}`
)

func completed(outputs string) sseEvent {
	return ev("response.completed", fmt.Sprintf(
		`"response":{"id":"resp_1","model":"gpt-real","status":"completed","output":[%s],"usage":{"input_tokens":20,"output_tokens":8,"total_tokens":28,"input_tokens_details":{"cached_tokens":5},"output_tokens_details":{"reasoning_tokens":3}}}`, outputs))
}

func TestStreamFullResponseWithAllItemTypes(t *testing.T) {
	client := newTestClient(t, Config{Model: ModelGPT6Luna}, serveEvents(
		ev("response.created", created),
		ev("response.output_item.added", `"output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}`),
		ev("response.reasoning_summary_part.added", `"item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}`),
		ev("response.reasoning_summary_text.delta", `"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"先"`),
		ev("response.reasoning_summary_text.delta", `"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"想"`),
		ev("response.reasoning_summary_part.added", `"item_id":"rs_1","output_index":0,"summary_index":1,"part":{"type":"summary_text","text":""}`),
		ev("response.reasoning_summary_text.delta", `"item_id":"rs_1","output_index":0,"summary_index":1,"delta":"二"`),
		ev("response.output_item.done", `"output_index":0,"item":`+reasoningRaw),
		ev("response.output_item.added", `"output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}`),
		ev("response.output_text.delta", `"item_id":"msg_1","output_index":1,"content_index":0,"delta":"我来"`),
		ev("response.output_text.delta", `"item_id":"msg_1","output_index":1,"content_index":0,"delta":"查"`),
		ev("response.output_item.done", `"output_index":1,"item":`+messageRaw),
		ev("response.output_item.added", `"output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}`),
		ev("response.function_call_arguments.delta", `"item_id":"fc_1","output_index":2,"delta":"{\"q\":"`),
		ev("response.function_call_arguments.delta", `"item_id":"fc_1","output_index":2,"delta":"\"a\"}"`),
		ev("response.output_item.done", `"output_index":2,"item":`+callRaw),
		ev("response.output_item.added", `"output_index":3,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"noargs","arguments":""}`),
		ev("response.output_item.done", `"output_index":3,"item":`+call2Raw),
		ev("response.output_item.done", `"output_index":4,"item":`+imageRaw),
		completed(reasoningRaw+","+messageRaw+","+callRaw+","+call2Raw+","+imageRaw),
	))

	events := run(t, client)
	final := events[len(events)-1]
	if final.Type != chat.EventDone || final.Reason != chat.StopReasonToolUse {
		t.Fatalf("wrong terminal event: %+v", final)
	}
	msg := final.Message
	wantTypes := []chat.ContentType{chat.ContentTypeThinking, chat.ContentTypeText, chat.ContentTypeToolCall, chat.ContentTypeToolCall, chat.ContentTypeMedia}
	if len(msg.Content) != len(wantTypes) {
		t.Fatalf("wrong number of content blocks (completed should not refill after done): %+v", msg.Content)
	}
	for i, want := range wantTypes {
		if msg.Content[i].Type != want {
			t.Fatalf("block %d has the wrong type: got=%s want=%s", i, msg.Content[i].Type, want)
		}
	}
	thinking := msg.Content[0]
	if thinking.Text != "先想\n\n二" || !strings.Contains(thinking.Signature, `"encrypted_content":"ENC"`) || !strings.Contains(thinking.Signature, `"id":"rs_1"`) {
		t.Fatalf("the thinking block should join the summary parts and keep the full reasoning item: %+v", thinking)
	}
	if msg.Content[1].Text != "我来查" || decodeTextSignature(msg.Content[1].Signature).ID != "msg_1" || decodeTextSignature(msg.Content[1].Signature).Phase != "commentary" {
		t.Fatalf("wrong text block or signature: %+v", msg.Content[1])
	}
	calls := msg.ToolCalls()
	if len(calls) != 2 || calls[0].ID != "call_1" || string(calls[0].Arguments) != `{"q":"a"}` || string(calls[1].Arguments) != `{}` {
		t.Fatalf("wrong tool calls: %+v", calls)
	}
	if msg.Content[2].Signature != "fc_1" || msg.Content[3].Signature != "fc_2" {
		t.Fatalf("the tool call block should keep the function_call item id: %+v", msg.Content)
	}
	if img := msg.Content[4]; img.MIMEType != "image/png" || string(img.Data) != "img" {
		t.Fatalf("wrong generated image block: %+v", img)
	}
	if msg.ResponseID != "resp_1" || msg.ResponseModel != "gpt-real" || msg.Provider != "openai" {
		t.Fatalf("wrong response metadata: %+v", msg)
	}
	if msg.Usage.Input != 15 || msg.Usage.CacheRead != 5 || msg.Usage.Output != 8 || msg.Usage.TotalTokens != 28 {
		t.Fatalf("wrong usage: %+v", msg.Usage)
	}
}

func TestStreamReconcilesItemsWithoutDoneEvents(t *testing.T) {
	// Endpoints that omit output_item.done (such as DeepSeek): items are filled in from the final output of response.completed.
	deepseekReasoning := `{"id":"rs_ds","type":"reasoning","content":[{"type":"reasoning_text","text":"明文推理"}]}`
	client := newTestClient(t, Config{Model: ModelGPT6Luna, DisableStoreParams: true}, serveEvents(
		ev("response.created", created),
		ev("response.output_item.added", `"output_index":0,"item":{"id":"rs_ds","type":"reasoning"}`),
		ev("response.reasoning_text.delta", `"item_id":"rs_ds","output_index":0,"content_index":0,"delta":"明文"`),
		ev("response.reasoning_text.delta", `"item_id":"rs_ds","output_index":0,"content_index":0,"delta":"推理"`),
		ev("response.output_item.added", `"output_index":1,"item":{"id":"msg_ds","type":"message","role":"assistant","content":[]}`),
		ev("response.output_text.delta", `"item_id":"msg_ds","output_index":1,"content_index":0,"delta":"结果"`),
		ev("response.output_item.added", `"output_index":2,"item":{"id":"fc_ds","type":"function_call","call_id":"call_ds","name":"lookup","arguments":""}`),
		completed(deepseekReasoning+`,{"id":"msg_ds","type":"message","role":"assistant","content":[{"type":"output_text","text":"结果"}]},{"id":"fc_ds","type":"function_call","call_id":"call_ds","name":"lookup","arguments":"{\"q\":1}"}`),
	))

	final := run(t, client)
	msg := final[len(final)-1].Message
	if len(msg.Content) != 3 {
		t.Fatalf("should fill in 3 blocks: %+v", msg.Content)
	}
	if msg.Content[0].Text != "明文推理" || !strings.Contains(msg.Content[0].Signature, `"reasoning_text"`) {
		t.Fatalf("the reasoning block should keep the full reasoning item so it can be sent back whole: %+v", msg.Content[0])
	}
	if msg.Content[1].Text != "结果" {
		t.Fatalf("text should not be duplicated: %+v", msg.Content[1])
	}
	if calls := msg.ToolCalls(); len(calls) != 1 || string(calls[0].Arguments) != `{"q":1}` || calls[0].ID != "call_ds" {
		t.Fatalf("tool call arguments should come from the final output: %+v", calls)
	}
}

func TestStreamTruncatedIsRetryableFailureWithPartialContent(t *testing.T) {
	client := newTestClient(t, Config{Model: ModelGPT6Luna}, serveEvents(
		ev("response.created", created),
		ev("response.output_item.added", `"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}`),
		ev("response.output_text.delta", `"item_id":"msg_1","output_index":0,"content_index":0,"delta":"说到一半"`),
		ev("response.output_item.added", `"output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}`),
	))
	events := run(t, client)
	final := events[len(events)-1]
	if final.Type != chat.EventError || !chat.AsError(final.Err).Retryable() {
		t.Fatalf("a dropped stream should be a retryable error: %+v", final)
	}
	if len(final.Message.Content) != 1 || final.Message.Content[0].Text != "说到一半" {
		t.Fatalf("should keep generated text and drop the incomplete tool call: %+v", final.Message.Content)
	}
}

func TestStreamFailedAndErrorEventsAreClassified(t *testing.T) {
	tests := []struct {
		name   string
		events []sseEvent
		want   chat.ErrorKind
	}{
		{"rate limit", []sseEvent{ev("response.created", created), ev("response.failed", `"response":{"id":"resp_1","status":"failed","error":{"code":"rate_limit_exceeded","message":"Rate limit reached"}}`)}, chat.KindRateLimit},
		{"server error", []sseEvent{ev("response.created", created), ev("response.failed", `"response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"The server had an error"}}`)}, chat.KindServer},
		{"context overflow", []sseEvent{ev("response.created", created), ev("response.failed", `"response":{"id":"resp_1","status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model"}}`)}, chat.KindContextLength},
		{"error event", []sseEvent{ev("response.created", created), ev("error", `"code":"insufficient_quota","message":"You exceeded your current quota"`)}, chat.KindQuota},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := run(t, newTestClient(t, Config{Model: ModelGPT6Luna}, serveEvents(tt.events...)))
			final := events[len(events)-1]
			if final.Type != chat.EventError || final.Message.ErrorKind != tt.want {
				t.Fatalf("got=%+v want=%s", final.Message.ErrorKind, tt.want)
			}
		})
	}
}

func TestStreamIncompleteAndRefusal(t *testing.T) {
	incomplete := func(reason string) sseEvent {
		return ev("response.incomplete", fmt.Sprintf(`"response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"%s"},"output":[]}`, reason))
	}
	events := run(t, newTestClient(t, Config{Model: ModelGPT6Luna}, serveEvents(ev("response.created", created), incomplete("max_output_tokens"))))
	if final := events[len(events)-1]; final.Type != chat.EventDone || final.Reason != chat.StopReasonLength {
		t.Fatalf("max_output_tokens should be length: %+v", final)
	}

	events = run(t, newTestClient(t, Config{Model: ModelGPT6Luna}, serveEvents(ev("response.created", created), incomplete("content_filter"))))
	if final := events[len(events)-1]; final.Type != chat.EventError || final.Message.ErrorKind != chat.KindRefusal {
		t.Fatalf("content_filter should be a refusal: %+v", final)
	}

	events = run(t, newTestClient(t, Config{Model: ModelGPT6Luna}, serveEvents(
		ev("response.created", created),
		ev("response.refusal.delta", `"item_id":"msg_1","output_index":0,"content_index":0,"delta":"我不能帮助"`),
		completed(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"我不能帮助"}]}`),
	)))
	final := events[len(events)-1]
	if final.Type != chat.EventError || final.Message.ErrorKind != chat.KindRefusal || !strings.Contains(final.Message.ErrorMessage, "我不能帮助") {
		t.Fatalf("a model refusal should be a non-retryable error carrying the reason: %+v", final)
	}
}

func TestStreamHTTPErrorIsClassified(t *testing.T) {
	client := newTestClient(t, Config{Model: ModelGPT6Luna}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`)
	})
	events := run(t, client)
	final := events[len(events)-1]
	if final.Type != chat.EventError || !errors.Is(final.Err, chat.ErrUnauthorized) || chat.AsError(final.Err).Status != 401 {
		t.Fatalf("401 should be an auth failure and keep the status code: %+v", final)
	}

	client = newTestClient(t, Config{Model: ModelGPT6Luna}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"Your input exceeds the context window of this model.","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	})
	events = run(t, client)
	if final := events[len(events)-1]; !errors.Is(final.Err, chat.ErrContextLengthExceeded) {
		t.Fatalf("should be recognized as context overflow: %+v", final.Err)
	}
}

func TestStreamCancellationIsAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	client := newTestClient(t, Config{Model: ModelGPT6Luna}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: "+ev("response.created", created).data+"\n\n")
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

func TestRequestIsSentToResponsesEndpoint(t *testing.T) {
	var path, auth string
	client := newTestClient(t, Config{Model: ModelGPT6Luna}, func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		serveEvents(ev("response.created", created), completed(""))(w, r)
	})
	run(t, client)
	if path != "/responses" || auth != "Bearer test" {
		t.Fatalf("should request /responses with Bearer auth: path=%s auth=%s", path, auth)
	}
}
