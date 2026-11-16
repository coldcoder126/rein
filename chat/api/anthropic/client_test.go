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

func TestNewValidatesTheConfig(t *testing.T) {
	if client, err := New(Config{APIKey: "test"}); err == nil || client != nil {
		t.Fatalf("New without a model should fail: %v", err)
	}
	if _, err := New(Config{Model: Model{Name: "m"}, Endpoint: Endpoint{Auth: "basic"}}); err == nil {
		t.Fatal("New with an unknown auth scheme should fail")
	}
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

func TestInfoReportsImageSupport(t *testing.T) {
	info := mustNew(Config{Model: sonnet55}).Info()
	if !info.SupportsMedia || info.ImageLimits.MaxBytes == 0 {
		t.Fatalf("should support media by default and give image specs: %+v", info)
	}
	if mustNew(Config{Model: sonnet55, Endpoint: Endpoint{Compat: Compat{NoMedia: true}}}).Info().SupportsMedia {
		t.Fatal("TextOnly should turn media support off")
	}
}
