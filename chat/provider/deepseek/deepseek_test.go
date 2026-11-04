package deepseek

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"

	anthropicprovider "github.com/coldcoder126/rein/chat/provider/anthropic"
	openaiprovider "github.com/coldcoder126/rein/chat/provider/openai"
)

func TestNewAnthropicDefaults(t *testing.T) {
	info := NewAnthropic(anthropicprovider.Config{Model: ModelDeepSeekFlash}).Info()
	if info.Model != ModelDeepSeekFlash || info.MaxInputTokens != 256_000 || info.MaxOutputTokens != 384_000 ||
		info.Provider != "deepseek" || info.API != anthropicprovider.API {
		t.Fatalf("wrong preset: %+v", info)
	}
	if !info.SupportsMedia || info.FileScope != "" {
		t.Fatalf("media should be passed through and files not used: %+v", info)
	}
	if NewAnthropic(anthropicprovider.Config{Model: ModelDeepSeekV4Pro, TextOnly: true}).Info().SupportsMedia {
		t.Fatal("TextOnly should be honored")
	}
}

func TestNewResponsesDefaults(t *testing.T) {
	info := NewResponses(openaiprovider.Config{Model: ModelDeepSeekFlash}).Info()
	if info.Model != ModelDeepSeekFlash || info.MaxInputTokens != 1_000_000 || info.MaxOutputTokens != 384_000 || info.Provider != "deepseek" || info.API != openaiprovider.API {
		t.Fatalf("wrong preset: %+v", info)
	}
	if !info.SupportsMedia || info.StructuredOutput || info.FileScope != "" {
		t.Fatalf("media should be passed through, structured output and files off: %+v", info)
	}
	if NewResponses(openaiprovider.Config{Model: ModelDeepSeekV4Pro, TextOnly: true}).Info().SupportsMedia {
		t.Fatal("TextOnly should be honored")
	}
}

func TestRepliesAndErrorsNameDeepSeek(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	}))
	defer server.Close()

	client := NewAnthropic(anthropicprovider.Config{Model: ModelDeepSeekFlash, APIKey: "test", BaseURL: server.URL})
	var last chat.Event
	for event := range client.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}) {
		last = event
	}
	if last.Type != chat.EventError || last.Message.Provider != "deepseek" || last.Message.API != anthropicprovider.API ||
		!strings.HasPrefix(last.Err.Error(), "deepseek") {
		t.Fatalf("the reply and its error should name DeepSeek: %+v %v", last.Message, last.Err)
	}
}
