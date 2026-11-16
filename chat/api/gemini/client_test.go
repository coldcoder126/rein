package gemini

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/genai"
)

func TestNewRequiresAModel(t *testing.T) {
	if client, err := New(Config{APIKey: "test"}); err == nil || client != nil {
		t.Fatalf("New without a model should fail: %v", err)
	}
}

func TestInfoDescribesEndpointAndModel(t *testing.T) {
	compatible := Endpoint{Provider: "compat", BaseURL: "https://compat.example"}
	info := mustNew(Config{Endpoint: compatible, Model: Model{Name: "m", MaxInputTokens: 1_000_000, MaxOutputTokens: 64_000}}).Info()
	if info.MaxInputTokens != 1_000_000 || info.MaxOutputTokens != 64_000 || info.Provider != "compat" || info.API != API || info.Model != "m" {
		t.Fatalf("the endpoint should set the provider and the model its limits: %+v", info)
	}
	if info.FileScope != "" {
		t.Fatalf("a custom endpoint has no Files API by default: %+v", info)
	}
	if info := mustNew(Config{Model: Model{Name: "m"}}).Info(); info.Provider != providerName || info.MaxInputTokens != 0 || info.FileScope != providerName {
		t.Fatalf("the zero endpoint is Google's Gemini API, and a model declares no limits by default: %+v", info)
	}
}

func TestEndpointReachesTheSDK(t *testing.T) {
	cases := []struct {
		cfg      Config
		backend  genai.Backend
		attempts int32
	}{
		{Config{Model: flash35}, genai.BackendGeminiAPI, 0},
		{Config{Model: flash35, Endpoint: Endpoint{Vertex: true}, MaxRetries: 2}, genai.BackendVertexAI, 3},
	}
	for _, tc := range cases {
		tc.cfg.APIKey = "test"
		sdk, err := mustNew(tc.cfg).newSDKClient(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		cc := sdk.ClientConfig()
		var attempts int32
		if r := cc.HTTPOptions.RetryOptions; r != nil {
			attempts = *r.Attempts
		}
		if cc.Backend != tc.backend || attempts != tc.attempts {
			t.Fatalf("%+v: backend %v, attempts %d", tc.cfg, cc.Backend, attempts)
		}
	}
}

func TestErrorsNameTheProvider(t *testing.T) {
	client := mustNew(Config{Endpoint: Endpoint{Provider: "compat"}, Model: flash35})
	if ce := client.wrapError(context.Background(), genai.APIError{Code: http.StatusInternalServerError}); ce.Message != "compat" {
		t.Fatalf("message = %q, want the provider", ce.Message)
	}
}
