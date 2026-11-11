package gemini

import (
	"context"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

func TestFileScope(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{Model: ModelGemini3_5Flash}, "gemini"},
		{Config{Model: ModelGemini3_5Flash, Vertex: true}, ""},
		{Config{Model: ModelGemini3_5Flash, BaseURL: "https://proxy.example.com"}, ""},
		{Config{Model: ModelGemini3_5Flash, FileScope: "gemini/prod"}, "gemini/prod"},
	} {
		if got := New(tc.cfg).Info().FileScope; got != tc.want {
			t.Fatalf("%+v: FileScope = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

func TestBuildRequestReferencesFiles(t *testing.T) {
	uri := "https://generativelanguage.googleapis.com/v1beta/files/abc"
	contents, _, err := New(Config{Model: ModelGemini3_5Flash}).buildRequest(chat.Request{Messages: []chat.Message{{Role: chat.RoleUser,
		Content: []chat.ContentBlock{chat.MediaURL("https://example.com/a.mp4").WithFile(chat.FileRef{Scope: "gemini", ID: uri})}}}})
	if err != nil {
		t.Fatal(err)
	}
	if data := contents[0].Parts[0].FileData; data == nil || data.FileURI != uri || data.MIMEType != "video/mp4" {
		t.Fatalf("files should be referred to by URI: %+v", contents[0].Parts[0])
	}
}

func TestUploadFileChecksArguments(t *testing.T) {
	ctx := context.Background()
	if _, err := New(Config{Model: ModelGemini3_5Flash, Vertex: true}).UploadFile(ctx, "a", "text/plain", strings.NewReader(""), 0); err == nil {
		t.Fatal("Vertex has no Files API")
	}
	if _, err := New(Config{Model: ModelGemini3_5Flash}).UploadFile(ctx, "a", "text/plain", strings.NewReader(""), 3600e9); err == nil {
		t.Fatal("the Gemini API takes no ttl")
	}
}
