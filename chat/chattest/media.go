package chattest

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/coldcoder126/rein/chat"
)

// TextPDF builds a minimal PDF with a single line of text; line must be ASCII.
func TextPDF(line string) []byte {
	stream := fmt.Sprintf("BT /F1 18 Tf 40 100 Td (%s) Tj ET", line)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 400 200] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return buf.Bytes()
}

// SampleImageURL is a public image (the Go logo) for testing URL media input.
// Whether a provider can fetch it is up to the provider: some sites reject them via robots.txt or access rules, so verify every provider when changing it.
const SampleImageURL = "https://raw.githubusercontent.com/github/explore/main/topics/go/go.png"

// CheckMediaInput tests media input against a real model: a URL image and an inline PDF go in with the user message,
// then a tool result returns another PDF. Both answers are logged and checked.
func CheckMediaInput(t *testing.T, client chat.Client) {
	t.Helper()
	ctx := t.Context()
	first, err := chat.Complete(ctx, client, chat.Request{
		Options: chat.Options{MaxTokens: 1024},
		Messages: []chat.Message{{Role: chat.RoleUser, CreatedAt: time.Now(), Content: []chat.ContentBlock{
			chat.Text("Describe the image in one sentence, then give the order number in the PDF."),
			chat.MediaURL(SampleImageURL),
			chat.Media(TextPDF("Order number: ZX-4471")).WithName("order.pdf"),
		}}},
	})
	if err != nil {
		t.Fatalf("user message with media: %v", err)
	}
	t.Logf("answer: %s", first.TextContent())
	if !bytes.Contains([]byte(first.TextContent()), []byte("ZX-4471")) {
		t.Errorf("the model should read the order number from the PDF: %q", first.TextContent())
	}

	call := chat.ToolCall{ID: "call_export_1", Name: "export_report", Arguments: []byte(`{}`)}
	second, err := chat.Complete(ctx, client, chat.Request{
		Options: chat.Options{MaxTokens: 1024},
		Tools:   []chat.ToolDef{{Name: "export_report", Description: "Export the quarterly report", InputSchema: []byte(`{"type":"object","properties":{}}`)}},
		Messages: []chat.Message{
			chat.NewUserTextMessage("Call export_report, then tell me the quarterly revenue in the report."),
			{Role: chat.RoleAssistant, Provider: "other", Model: "other", StopReason: chat.StopReasonToolUse,
				Content: []chat.ContentBlock{{Type: chat.ContentTypeToolCall, ToolCall: &call}}},
			{Role: chat.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: []chat.ContentBlock{
				chat.Text("The report has been exported, see the attachment."),
				chat.Media(TextPDF("Quarterly revenue: 8842 million")).WithName("report.pdf"),
			}},
		},
	})
	if err != nil {
		t.Fatalf("tool result with media: %v", err)
	}
	t.Logf("answer: %s", second.TextContent())
	if !bytes.Contains([]byte(second.TextContent()), []byte("8842")) && !bytes.Contains([]byte(second.TextContent()), []byte("8,842")) {
		t.Errorf("the model should read the PDF returned by the tool: %q", second.TextContent())
	}
}
