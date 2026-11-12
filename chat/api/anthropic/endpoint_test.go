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

// compatible is an Anthropic-compatible endpoint as a caller would describe it.
var compatible = Endpoint{
	Provider:        "compat",
	BaseURL:         "https://compat.example/anthropic",
	MaxInputTokens:  256_000,
	MaxOutputTokens: 384_000,
	Compat:          Compat{NoThinkingSignature: true},
}

func TestEndpointLimitsAndCompat(t *testing.T) {
	info := mustNew(Config{Endpoint: compatible, Model: "m"}).Info()
	if info.MaxInputTokens != 256_000 || info.MaxOutputTokens != 384_000 || info.Provider != "compat" || info.API != API {
		t.Fatalf("the endpoint should set provider and limits: %+v", info)
	}
	if !info.SupportsMedia || info.FileScope != "" {
		t.Fatalf("media should be passed through and files not used: %+v", info)
	}
	// Config limits take precedence over the endpoint's.
	endpoint := compatible
	endpoint.Compat.NoMedia = true
	info = mustNew(Config{Endpoint: endpoint, Model: "m", MaxInputTokens: 1000}).Info()
	if info.SupportsMedia || info.MaxInputTokens != 1000 || info.MaxOutputTokens != 384_000 {
		t.Fatalf("Compat and Config limits should be honored: %+v", info)
	}
}

func TestRepliesAndErrorsNameTheEndpointProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	}))
	defer server.Close()

	endpoint := compatible
	endpoint.BaseURL = server.URL
	client := mustNew(Config{Endpoint: endpoint, Model: "m", APIKey: "test"})
	var last chat.Event
	for event := range client.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}) {
		last = event
	}
	if last.Type != chat.EventError || last.Message.Provider != "compat" || last.Message.API != API ||
		!strings.HasPrefix(last.Err.Error(), "compat") {
		t.Fatalf("the reply and its error should name the endpoint's provider: %+v %v", last.Message, last.Err)
	}
}
