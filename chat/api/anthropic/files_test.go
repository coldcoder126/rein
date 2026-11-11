package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

func TestFileScope(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{Model: ModelClaudeSonnet5_5}, "anthropic"},
		{Config{Model: ModelClaudeSonnet5_5, Endpoint: Endpoint{BaseURL: "https://proxy.example.com"}}, ""},
		{Config{Model: ModelClaudeSonnet5_5, FileScope: "anthropic/prod"}, "anthropic/prod"},
	} {
		if got := mustNew(tc.cfg).Info().FileScope; got != tc.want {
			t.Fatalf("%+v: FileScope = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

func TestBuildParamsReferencesFiles(t *testing.T) {
	ref := chat.FileRef{Scope: "anthropic", ID: "file_1"}
	got := paramsJSON(t, mustNew(Config{Model: ModelClaudeSonnet5_5}), chat.Request{Messages: []chat.Message{{Role: chat.RoleUser, Content: []chat.ContentBlock{
		chat.MediaFile(ref, "image/png"),
		chat.MediaURL("https://example.com/a.pdf").WithName("a.pdf").WithFile(ref),
	}}}})
	if !strings.Contains(got, `"source":{"file_id":"file_1","type":"file"},"type":"image"`) ||
		!strings.Contains(got, `"source":{"file_id":"file_1","type":"file"},"title":"a.pdf"`) || strings.Contains(got, "example.com") {
		t.Fatalf("files should be referred to by ID: %s", got)
	}
}

func TestUploadFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/files" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		body, _ := io.ReadAll(file)
		if string(body) != "%PDF" || header.Filename != "a.pdf" || r.FormValue("expires_in_seconds") != "3600" {
			t.Errorf("wrong upload: %q %q %q", body, header.Filename, r.FormValue("expires_in_seconds"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"file_1","type":"file","filename":"a.pdf","mime_type":"application/pdf","size_bytes":4,
			"created_at":"2026-01-01T00:00:00Z","downloadable":false,"expires_at":"2026-01-01T01:00:00Z"}`)
	}))
	defer server.Close()

	client := mustNew(Config{Model: ModelClaudeSonnet5_5, APIKey: "test", Endpoint: Endpoint{BaseURL: server.URL}, FileScope: "test"})
	block, err := client.UploadFile(context.Background(), "a.pdf", "application/pdf", strings.NewReader("%PDF"), 3600e9)
	if err != nil {
		t.Fatal(err)
	}
	if block.File == nil || block.File.ID != "file_1" || block.File.Scope != "test" || block.File.ExpiresAt.IsZero() ||
		block.MIMEType != "application/pdf" || block.Name != "a.pdf" {
		t.Fatalf("wrong block: %+v %+v", block, block.File)
	}
	if _, err := mustNew(Config{Model: ModelClaudeSonnet5_5, Endpoint: Endpoint{BaseURL: server.URL}}).UploadFile(context.Background(), "a", "text/plain", strings.NewReader(""), 0); err == nil {
		t.Fatal("an endpoint without a file store should refuse uploads")
	}
}
