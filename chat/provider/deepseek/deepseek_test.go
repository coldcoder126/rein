package deepseek

import (
	"testing"

	anthropicprovider "github.com/coldcoder126/rein/chat/provider/anthropic"
	openaiprovider "github.com/coldcoder126/rein/chat/provider/openai"
)

func TestNewAnthropicDefaults(t *testing.T) {
	info := NewAnthropic(anthropicprovider.Config{Model: ModelDeepSeekFlash}).Info()
	if info.Model != ModelDeepSeekFlash || info.MaxInputTokens != 256_000 {
		t.Fatalf("wrong preset: %+v", info)
	}
	if !info.SupportsMedia || info.FileScope != "" {
		t.Fatalf("media should be passed through and files not used: %+v", info)
	}
	if NewAnthropic(anthropicprovider.Config{Model: ModelDeepSeekV4Pro, TextOnly: true}).Info().SupportsMedia {
		t.Fatal("TextOnly should be honored")
	}
}

func TestNewResponsesDefaults(t *testing.T) {
	info := NewResponses(openaiprovider.Config{Model: ModelDeepSeekFlash}).Info()
	if info.Model != ModelDeepSeekFlash || info.MaxInputTokens != 1_000_000 || info.MaxOutputTokens != 384_000 || info.Provider != "openai" {
		t.Fatalf("wrong preset: %+v", info)
	}
	if !info.SupportsMedia || info.StructuredOutput || info.FileScope != "" {
		t.Fatalf("media should be passed through, structured output and files off: %+v", info)
	}
	if NewResponses(openaiprovider.Config{Model: ModelDeepSeekV4Pro, TextOnly: true}).Info().SupportsMedia {
		t.Fatal("TextOnly should be honored")
	}
}
