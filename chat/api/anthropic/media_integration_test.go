package anthropic

import (
	"testing"

	"github.com/coldcoder126/rein/chat/chattest"
)

// A real model reads a URL image, an inline PDF, and a PDF in a tool result.
func TestIntegrationMedia(t *testing.T) {
	chattest.CheckMediaInput(t, integrationClient(t, Config{Model: ModelClaudeHaiku4_5}))
}
