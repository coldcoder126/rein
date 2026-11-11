package openai

import "testing"

// mustNew creates a client for a test whose configuration is known to be valid.
func mustNew(cfg Config) *Client {
	client, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return client
}

func TestNewRequiresAModel(t *testing.T) {
	if client, err := New(Config{APIKey: "test"}); err == nil || client != nil {
		t.Fatalf("New without a model should fail: %v", err)
	}
}
