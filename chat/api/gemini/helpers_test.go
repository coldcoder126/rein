package gemini

// mustNew creates a client for a test whose configuration is known to be valid.
func mustNew(cfg Config) *Client {
	client, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return client
}

// The models the tests talk to, described as a caller would.
var (
	neon4   = Model{Name: "gemini-4-neon", MaxInputTokens: 1_048_576, MaxOutputTokens: 1_048_576}
	flash35 = Model{Name: "gemini-3.5-flash", MaxInputTokens: 1_048_576, MaxOutputTokens: 65_536, Compat: gemini3}
	pro31   = Model{Name: "gemini-3.1-pro-preview", MaxInputTokens: 1_048_576, MaxOutputTokens: 65_536, Compat: gemini3}
	flash25 = Model{Name: "gemini-2.5-flash", MaxInputTokens: 1_048_576, MaxOutputTokens: 65_536, Compat: gemini2}
	// gemini3 is what the Gemini 3.x generation lacks.
	gemini3 = Compat{NoXHighThinking: true}
	// gemini2 is what the Gemini 2.x generation lacks.
	gemini2 = Compat{NoThinkingLevel: true, NoXHighThinking: true, NoMediaInToolResults: true}
)
