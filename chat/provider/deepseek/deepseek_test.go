package deepseek

import (
	"testing"

	anthropicprovider "github.com/coldcoder126/rein/chat/provider/anthropic"
	openaiprovider "github.com/coldcoder126/rein/chat/provider/openai"
)

func TestNewAnthropicDefaults(t *testing.T) {
	client := NewAnthropic(anthropicprovider.Config{})
	info := client.Info()
	if info.Model != ModelDeepSeekV4Flash {
		t.Fatalf("wrong default model: %s", info.Model)
	}
	if info.MaxInputTokens != 256_000 {
		t.Fatalf("wrong default context window: %d", info.MaxInputTokens)
	}
	if info.SupportsMedia {
		t.Fatal("DeepSeek's Anthropic-compatible endpoint does not support images")
	}
}

func TestNewResponsesDefaults(t *testing.T) {
	client := NewResponses(openaiprovider.Config{})
	info := client.Info()
	if info.Model != ModelDeepSeekV4Flash {
		t.Fatalf("wrong default model: %s", info.Model)
	}
	if info.MaxInputTokens != 1_000_000 {
		t.Fatalf("wrong default context window: %d", info.MaxInputTokens)
	}
	if info.MaxOutputTokens != 384_000 {
		t.Fatalf("wrong default output limit: %d", info.MaxOutputTokens)
	}
	if info.SupportsMedia || info.Provider != "openai" {
		t.Fatalf("the DeepSeek Responses preset should be text only: %+v", info)
	}
}
