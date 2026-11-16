package anthropic

import (
	"testing"
)

// compatible is an Anthropic-compatible endpoint as a caller would describe it.
var compatible = Endpoint{
	Provider: "compat",
	BaseURL:  "https://compat.example/anthropic",
	Compat:   Compat{NoThinkingSignature: true},
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
