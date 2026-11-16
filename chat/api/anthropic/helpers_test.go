package anthropic

// mustNew creates a client for a test whose configuration is known to be valid.
func mustNew(cfg Config) *Client {
	client, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return client
}

// The models the tests talk to, described as a caller would: the current generation lacks nothing but turning thinking
// off, and older generations and compatible models say what they lack.
var (
	opus55   = Model{Name: "claude-opus-5-5", MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000, Compat: Compat{NoDisableThinking: true}}
	sonnet55 = Model{Name: "claude-sonnet-5-5", MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000, Compat: Compat{NoDisableThinking: true}}
	haiku55  = Model{Name: "claude-haiku-5-5", MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000}
	sonnet46 = Model{Name: "claude-sonnet-4-6", MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000, Compat: Compat{NoXHigh: true, NoMaxEffort: true, NoThinkingBinding: true}}
	haiku45  = Model{Name: "claude-haiku-4-5", MaxInputTokens: 200_000, MaxOutputTokens: 64_000, Compat: Compat{NoAdaptiveThinking: true}}
	// thirdParty is a model of a compatible service that only knows the basic protocol.
	thirdParty = Model{Name: "deepseek-flash", MaxOutputTokens: 8_192, Compat: Compat{NoAdaptiveThinking: true, NoStructuredOutput: true, NoToolSearch: true}}
)
