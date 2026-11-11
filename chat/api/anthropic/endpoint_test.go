package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

func TestDeepSeekEndpoint(t *testing.T) {
	info := mustNew(Config{Endpoint: DeepSeek, Model: ModelDeepSeekFlash}).Info()
	if info.Model != ModelDeepSeekFlash || info.MaxInputTokens != 256_000 || info.MaxOutputTokens != 384_000 ||
		info.Provider != "deepseek" || info.API != API {
		t.Fatalf("wrong preset: %+v", info)
	}
	if !info.SupportsMedia || info.FileScope != "" {
		t.Fatalf("media should be passed through and files not used: %+v", info)
	}
	// Config limits take precedence over the endpoint's, and a copy of the preset can be adjusted.
	endpoint := DeepSeek
	endpoint.Compat.NoMedia = true
	info = mustNew(Config{Endpoint: endpoint, Model: ModelDeepSeekV4Pro, MaxInputTokens: 1000}).Info()
	if info.SupportsMedia || info.MaxInputTokens != 1000 || info.MaxOutputTokens != 384_000 {
		t.Fatalf("Compat and Config limits should be honored: %+v", info)
	}
	if DeepSeek.Compat.NoMedia {
		t.Fatal("adjusting a copy should not change the preset")
	}
}

func TestRepliesAndErrorsNameTheEndpointProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	}))
	defer server.Close()

	endpoint := DeepSeek
	endpoint.BaseURL = server.URL
	client := mustNew(Config{Endpoint: endpoint, Model: ModelDeepSeekFlash, APIKey: "test"})
	var last chat.Event
	for event := range client.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}) {
		last = event
	}
	if last.Type != chat.EventError || last.Message.Provider != "deepseek" || last.Message.API != API ||
		!strings.HasPrefix(last.Err.Error(), "deepseek") {
		t.Fatalf("the reply and its error should name DeepSeek: %+v %v", last.Message, last.Err)
	}
}
