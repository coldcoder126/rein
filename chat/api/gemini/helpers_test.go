package gemini

import "testing"

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
	flash35 = Model{Name: "gemini-3.5-flash", MaxInputTokens: 1_048_576, MaxOutputTokens: 65_536}
	pro31   = Model{Name: "gemini-3.1-pro-preview", MaxInputTokens: 1_048_576, MaxOutputTokens: 65_536}
	flash25 = Model{Name: "gemini-2.5-flash", MaxInputTokens: 1_048_576, MaxOutputTokens: 65_536, Compat: gemini2}
	// gemini2 is what the Gemini 2.x generation lacks.
	gemini2 = Compat{NoThinkingLevel: true, NoMediaInToolResults: true}
)

func TestNewRequiresAModel(t *testing.T) {
	if client, err := New(Config{APIKey: "test"}); err == nil || client != nil {
		t.Fatalf("New without a model should fail: %v", err)
	}
}
