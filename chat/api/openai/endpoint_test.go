package openai

import "testing"

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
