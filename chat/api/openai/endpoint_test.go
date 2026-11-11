package openai

import "testing"

func TestDeepSeekEndpoint(t *testing.T) {
	info := New(Config{Endpoint: DeepSeek, Model: ModelDeepSeekFlash}).Info()
	if info.Model != ModelDeepSeekFlash || info.MaxInputTokens != 1_000_000 || info.MaxOutputTokens != 384_000 ||
		info.Provider != "deepseek" || info.API != API {
		t.Fatalf("wrong preset: %+v", info)
	}
	if !info.SupportsMedia || info.StructuredOutput || info.FileScope != "" {
		t.Fatalf("media should be passed through, structured output and files off: %+v", info)
	}
	endpoint := DeepSeek
	endpoint.Compat.NoMedia = true
	info = New(Config{Endpoint: endpoint, Model: ModelDeepSeekV4Pro, MaxOutputTokens: 1000}).Info()
	if info.SupportsMedia || info.MaxInputTokens != 1_000_000 || info.MaxOutputTokens != 1000 {
		t.Fatalf("Compat and Config limits should be honored: %+v", info)
	}
}
