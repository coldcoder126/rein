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
	Provider: "compat",
	BaseURL:  "https://compat.example/anthropic",
	Compat:   Compat{NoThinkingSignature: true},
}

func TestInfoDescribesEndpointAndModel(t *testing.T) {
	info := mustNew(Config{Endpoint: compatible, Model: Model{Name: "m", MaxInputTokens: 256_000, MaxOutputTokens: 384_000}}).Info()
	if info.MaxInputTokens != 256_000 || info.MaxOutputTokens != 384_000 || info.Provider != "compat" || info.API != API || info.Model != "m" {
		t.Fatalf("the endpoint should set the provider and the model its limits: %+v", info)
	}
	if !info.SupportsMedia || !info.StructuredOutput || info.FileScope != "" {
		t.Fatalf("a model lacks nothing unless it says so, and a custom endpoint has no files: %+v", info)
	}
	if info := mustNew(Config{Model: Model{Name: "m"}}).Info(); info.MaxInputTokens != 0 || info.MaxOutputTokens != 0 {
		t.Fatalf("limits the model does not declare should stay undeclared: %+v", info)
	}
}

// A client lacks what either its endpoint or its model lacks.
func TestCompatOfEndpointAndModelAdd(t *testing.T) {
	endpoint := compatible
	endpoint.Compat.NoMedia = true
	client := mustNew(Config{Endpoint: endpoint, Model: Model{Name: "m", Compat: Compat{NoStructuredOutput: true}}})
	want := Compat{NoMedia: true, NoThinkingSignature: true, NoStructuredOutput: true}
	if client.compat != want {
		t.Fatalf("compat = %+v, want %+v", client.compat, want)
	}
	if info := client.Info(); info.SupportsMedia || info.StructuredOutput {
		t.Fatalf("Info should reflect what is lacking: %+v", info)
	}
}

// The key goes out the way the endpoint takes it, and never under the other scheme.
func TestAuthScheme(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "env-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-token")
	for _, tc := range []struct {
		auth           Auth
		apiKey, bearer string
	}{
		{AuthAPIKey, "test", ""},
		{AuthBearer, "", "Bearer test"},
	} {
		var apiKey, bearer string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey, bearer = r.Header.Get("X-Api-Key"), r.Header.Get("Authorization")
			w.WriteHeader(http.StatusBadRequest)
		}))
		client := mustNew(Config{Endpoint: Endpoint{BaseURL: server.URL, Auth: tc.auth}, Model: Model{Name: "m"}, APIKey: "test"})
		for range client.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}) {
		}
		server.Close()
		if apiKey != tc.apiKey || bearer != tc.bearer {
			t.Fatalf("auth %q: x-api-key = %q, authorization = %q", tc.auth, apiKey, bearer)
		}
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
	client := mustNew(Config{Endpoint: endpoint, Model: Model{Name: "m"}, APIKey: "test"})
	var last chat.Event
	for event := range client.Stream(context.Background(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}) {
		last = event
	}
	if last.Type != chat.EventError || last.Message.Provider != "compat" || last.Message.API != API ||
		!strings.HasPrefix(last.Err.Error(), "compat") {
		t.Fatalf("the reply and its error should name the endpoint's provider: %+v %v", last.Message, last.Err)
	}
}
