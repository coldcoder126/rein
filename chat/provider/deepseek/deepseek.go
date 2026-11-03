// Package deepseek provides constructors for DeepSeek chat.Clients.
package deepseek

import (
	anthropicprovider "github.com/coldcoder126/rein/chat/provider/anthropic"
	openaiprovider "github.com/coldcoder126/rein/chat/provider/openai"
)

const (
	BaseURLAnthropic     = "https://api.deepseek.com/anthropic"
	BaseURLOpenAI        = "https://api.deepseek.com"
	ModelDeepSeekV4Flash = "deepseek-v4-flash"
	ModelDeepSeekV4Pro   = "deepseek-v4-pro"
	ModelReasoner        = "deepseek-reasoner"
)

// NewAnthropic creates a client for DeepSeek's Anthropic-compatible endpoint.
func NewAnthropic(cfg anthropicprovider.Config) *anthropicprovider.Client {
	cfg.TextOnly = true
	cfg.UnsignedThinking = true
	if cfg.BaseURL == "" {
		cfg.BaseURL = BaseURLAnthropic
	}
	if cfg.Model == "" {
		cfg.Model = ModelDeepSeekV4Flash
	}
	if cfg.MaxInputTokens == 0 {
		cfg.MaxInputTokens = 256_000
	}
	return anthropicprovider.New(cfg)
}

// NewResponses creates a client for DeepSeek's Responses API.
//
// DeepSeek's Responses endpoint is stateless and does not recognize the store / include parameters; reasoning comes back as plain
// reasoning items that must be sent back whole. Image input is treated as text only; once the model is confirmed to support vision, build an openai.Config yourself to override it.
func NewResponses(cfg openaiprovider.Config) *openaiprovider.Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = BaseURLOpenAI
	}
	if cfg.Model == "" {
		cfg.Model = ModelDeepSeekV4Flash
	}
	if cfg.MaxInputTokens == 0 {
		cfg.MaxInputTokens = 1_000_000
	}
	if cfg.MaxOutputTokens == 0 {
		cfg.MaxOutputTokens = 384_000
	}
	cfg.DisableStoreParams = true
	cfg.TextOnly = true
	cfg.NoStructuredOutput = true
	return openaiprovider.New(cfg)
}
