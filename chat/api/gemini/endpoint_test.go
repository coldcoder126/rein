package gemini

import (
	"encoding/json"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

// A client lacks what either its endpoint or its model lacks, and requests follow that.
func TestCompatOfEndpointAndModelAdd(t *testing.T) {
	client := mustNew(Config{
		Endpoint: Endpoint{Compat: Compat{NoThinkingLevel: true, NoXHighThinking: true}},
		Model:    Model{Name: "m", Compat: Compat{NoMediaInToolResults: true}},
	})
	if want := gemini2; client.compat != want {
		t.Fatalf("compat = %+v, want %+v", client.compat, want)
	}
	_, config, err := client.buildRequest(chat.Request{Options: chat.Options{Reasoning: chat.ReasoningLow}})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(config.ThinkingConfig); string(raw) != `{"includeThoughts":true,"thinkingBudget":2048}` {
		t.Fatalf("the endpoint's NoThinkingLevel should ask for a budget: %s", raw)
	}
}
