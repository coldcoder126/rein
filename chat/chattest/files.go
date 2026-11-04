package chattest

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/coldcoder126/rein/chat"
)

// FileClient is a chat.Client with a file store, as the providers implement it.
type FileClient interface {
	chat.Client
	UploadFile(ctx context.Context, name, mimeType string, r io.Reader, ttl time.Duration) (chat.ContentBlock, error)
	DeleteFile(ctx context.Context, id string) error
}

// CheckFileInput uploads a PDF, has the model read it by reference, and deletes it.
func CheckFileInput(t *testing.T, client FileClient) {
	t.Helper()
	ctx := t.Context()
	block, err := client.UploadFile(ctx, "invoice.pdf", "application/pdf", bytes.NewReader(TextPDF("Invoice code: QF-2093")), 0)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() {
		if err := client.DeleteFile(context.Background(), block.File.ID); err != nil {
			t.Errorf("delete: %v", err)
		}
	})
	answer, err := chat.Complete(ctx, client, chat.Request{
		Options: chat.Options{MaxTokens: 1024},
		Messages: []chat.Message{{Role: chat.RoleUser, CreatedAt: time.Now(), Content: []chat.ContentBlock{
			chat.Text("What is the invoice code in the PDF? Reply with the code only."), block,
		}}},
	})
	if err != nil {
		t.Fatalf("message with an uploaded file: %v", err)
	}
	t.Logf("answer: %s", answer.TextContent())
	if !strings.Contains(answer.TextContent(), "QF-2093") {
		t.Errorf("the model should read the uploaded PDF: %q", answer.TextContent())
	}
}
