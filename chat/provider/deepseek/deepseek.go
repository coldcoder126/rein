// Package deepseek provides constructors for DeepSeek chat.Clients.
package deepseek

import (
	anthropicprovider "github.com/coldcoder126/rein/chat/provider/anthropic"
	openaiprovider "github.com/coldcoder126/rein/chat/provider/openai"
)

const (
	providerName = "deepseek"

	BaseURLAnthropic   = "https://api.deepseek.com/anthropic"
	BaseURLOpenAI      = "https://api.deepseek.com"
	ModelDeepSeekFlash = "deepseek-flash"
	ModelDeepSeekV4Pro = "deepseek-v4-pro"
)

// NewAnthropic creates a client for DeepSeek's Anthropic-compatible endpoint.
//
// Media is passed through as for any endpoint. DeepSeek reads images only, and not on every model; whether to send media is
// the caller's choice: set TextOnly to replace it with a note instead.
func NewAnthropic(cfg anthropicprovider.Config) *anthropicprovider.Client {
	if cfg.Provider == "" {
		cfg.Provider = providerName
	}
	cfg.UnsignedThinking = true
	if cfg.BaseURL == "" {
		cfg.BaseURL = BaseURLAnthropic
	}
	if cfg.MaxInputTokens == 0 {
		cfg.MaxInputTokens = 256_000
	}
	// The endpoint documents no max_tokens limit (it depends on model and thinking); use the model's output limit.
	if cfg.MaxOutputTokens == 0 {
		cfg.MaxOutputTokens = 384_000
	}
	return anthropicprovider.New(cfg)
}

// NewResponses creates a client for DeepSeek's Responses API.
//
// DeepSeek's Responses endpoint is stateless and does not recognize the store / include parameters; reasoning comes back as plain
// reasoning items that must be sent back whole. Media is handled as in NewAnthropic: set TextOnly when the model should not receive it.
func NewResponses(cfg openaiprovider.Config) *openaiprovider.Client {
	if cfg.Provider == "" {
		cfg.Provider = providerName
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = BaseURLOpenAI
	}
	if cfg.MaxInputTokens == 0 {
		cfg.MaxInputTokens = 1_000_000
	}
	if cfg.MaxOutputTokens == 0 {
		cfg.MaxOutputTokens = 384_000
	}
	cfg.DisableStoreParams = true
	cfg.NoStructuredOutput = true
	return openaiprovider.New(cfg)
}
