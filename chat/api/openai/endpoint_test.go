package openai

import "testing"

func TestEndpointLimitsAndCompat(t *testing.T) {
	compatible := Endpoint{
		Provider:        "compat",
		BaseURL:         "https://compat.example",
		MaxInputTokens:  1_000_000,
		MaxOutputTokens: 384_000,
		Compat:          Compat{NoStoreParams: true, NoStructuredOutput: true, NoToolSearch: true},
	}
	info := mustNew(Config{Endpoint: compatible, Model: "m"}).Info()
	if info.MaxInputTokens != 1_000_000 || info.MaxOutputTokens != 384_000 || info.Provider != "compat" || info.API != API {
		t.Fatalf("the endpoint should set provider and limits: %+v", info)
	}
	if !info.SupportsMedia || info.StructuredOutput || info.FileScope != "" {
		t.Fatalf("media should be passed through, structured output and files off: %+v", info)
	}
	compatible.Compat.NoMedia = true
	info = mustNew(Config{Endpoint: compatible, Model: "m", MaxOutputTokens: 1000}).Info()
	if info.SupportsMedia || info.MaxInputTokens != 1_000_000 || info.MaxOutputTokens != 1000 {
		t.Fatalf("Compat and Config limits should be honored: %+v", info)
	}
}
