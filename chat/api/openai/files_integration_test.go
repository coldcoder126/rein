package openai

import (
	"testing"

	"github.com/coldcoder126/rein/chat/chattest"
)

func TestIntegrationFiles(t *testing.T) {
	chattest.CheckFileInput(t, integrationClient(t, Config{Model: ModelGPT6Luna}))
}
