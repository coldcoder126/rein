package openai

import "testing"

func TestNewRequiresAModel(t *testing.T) {
	if client, err := New(Config{APIKey: "test"}); err == nil || client != nil {
		t.Fatalf("New without a model should fail: %v", err)
	}
}

func TestInfoDescribesEndpointAndModel(t *testing.T) {
	compatible := Endpoint{
		Provider: "compat",
		BaseURL:  "https://compat.example",
		Compat:   Compat{NoStoreParams: true, NoStructuredOutput: true, NoToolSearch: true},
	}
	info := mustNew(Config{Endpoint: compatible, Model: Model{Name: "m", MaxInputTokens: 1_000_000, MaxOutputTokens: 384_000}}).Info()
	if info.MaxInputTokens != 1_000_000 || info.MaxOutputTokens != 384_000 || info.Provider != "compat" || info.API != API || info.Model != "m" {
		t.Fatalf("the endpoint should set the provider and the model its limits: %+v", info)
	}
	if !info.SupportsMedia || info.StructuredOutput || info.FileScope != "" {
		t.Fatalf("media should be passed through, structured output and files off: %+v", info)
	}
	if info := mustNew(Config{Model: Model{Name: "m"}}).Info(); info.MaxInputTokens != 0 || info.MaxOutputTokens != 0 || !info.StructuredOutput {
		t.Fatalf("a model lacks nothing unless it says so, and declares no limits by default: %+v", info)
	}
}

func TestInfoImageReplayFollowsTextOnly(t *testing.T) {
	if !mustNew(Config{Model: plain}).Info().ImageOutput || mustNew(Config{Model: plain, Endpoint: Endpoint{Compat: Compat{NoMedia: true}}}).Info().ImageOutput {
		t.Fatal("ImageOutput should be off with TextOnly")
	}
}
