package openai

import "testing"

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

// A client lacks what either its endpoint or its model lacks.
func TestCompatOfEndpointAndModelAdd(t *testing.T) {
	client := mustNew(Config{
		Endpoint: Endpoint{Compat: Compat{NoStoreParams: true}},
		Model:    Model{Name: "m", Compat: Compat{NoMedia: true, NoReasoning: true}},
	})
	want := Compat{NoMedia: true, NoReasoning: true, NoStoreParams: true}
	if client.compat != want {
		t.Fatalf("compat = %+v, want %+v", client.compat, want)
	}
	if info := client.Info(); info.SupportsMedia || info.ImageOutput {
		t.Fatalf("Info should reflect what is lacking: %+v", info)
	}
}
