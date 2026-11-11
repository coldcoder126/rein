package gemini

import (
	"testing"

	"github.com/coldcoder126/rein/chat/chattest"
)

func TestIntegrationFiles(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelGemini3_5Flash})
	if client.Info().FileScope == "" {
		t.Skip("needs a Gemini API key: Vertex has no Files API")
	}
	chattest.CheckFileInput(t, client)
}
