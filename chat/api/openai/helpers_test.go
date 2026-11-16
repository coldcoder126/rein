package openai

// mustNew creates a client for a test whose configuration is known to be valid.
func mustNew(cfg Config) *Client {
	client, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return client
}

var (
	// luna is a current model, which lacks nothing.
	luna = Model{Name: "gpt-6-luna", MaxInputTokens: 128_000, MaxOutputTokens: 16_384}
	// plain is a model that does not reason.
	plain = Model{Name: "gpt-6-luna", MaxInputTokens: 128_000, MaxOutputTokens: 16_384, Compat: Compat{NoReasoning: true}}
)
