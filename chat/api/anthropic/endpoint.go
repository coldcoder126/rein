package anthropic

// Endpoint is a service that speaks the Anthropic Messages API: Anthropic itself (the zero value) or a compatible one.
// It states facts about the service; what the caller wants from it is set on Config.
type Endpoint struct {
	// Provider names the vendor in messages, usage and errors; "anthropic" by default.
	Provider string
	// BaseURL is the address of the API; empty for Anthropic's.
	BaseURL string
	// MaxInputTokens / MaxOutputTokens are the limits of the endpoint's models, used when Config sets none.
	// 0 uses the model's built-in specs.
	MaxInputTokens  int
	MaxOutputTokens int
	// Compat says where the endpoint differs from Anthropic's.
	Compat Compat
}

// Compat lists what a compatible endpoint lacks compared with Anthropic's own. The zero value lacks nothing.
type Compat struct {
	// NoMedia means the endpoint accepts no media input; media is replaced with a note before the request is sent.
	NoMedia bool
	// NoAdaptiveThinking means the endpoint only knows budget thinking (thinking.type=enabled with budget_tokens).
	// Claude models are configured from built-in specs and need no setting.
	NoAdaptiveThinking bool
	// NoThinkingSignature means the endpoint's thinking blocks carry no signature and are replayed as they are;
	// otherwise unsigned thinking blocks are replayed as text, because Anthropic rejects them.
	NoThinkingSignature bool
	// NoThinkingBinding means the endpoint does not accept thinking.block_binding or its beta header, so
	// Config.PrefixMismatch is not sent and the endpoint's own default applies.
	NoThinkingBinding bool
}
