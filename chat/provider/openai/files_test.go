package openai

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
		{Config{Model: ModelGPT6Luna}, "openai"},
		{Config{Model: ModelGPT6Luna, BaseURL: "https://api.deepseek.com"}, ""},
		{Config{Model: ModelGPT6Luna, FileScope: "openai/prod"}, "openai/prod"},
	} {
		if got := New(tc.cfg).Info().FileScope; got != tc.want {
			t.Fatalf("%+v: FileScope = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

func TestBuildParamsReferencesFiles(t *testing.T) {
	ref := chat.FileRef{Scope: "openai", ID: "file-1"}
	got := marshal(t, mustParams(t, New(Config{Model: ModelGPT6Luna}), chat.Request{Messages: []chat.Message{{Role: chat.RoleUser, Content: []chat.ContentBlock{
		chat.MediaFile(ref, "image/png"),
		chat.MediaURL("https://example.com/a.pdf").WithFile(ref),
	}}}}))
	if !strings.Contains(got, `{"detail":"auto","file_id":"file-1","type":"input_image"}`) ||
		!strings.Contains(got, `{"file_id":"file-1","type":"input_file"}`) || strings.Contains(got, "example.com") {
		t.Fatalf("files should be referred to by ID: %s", got)
	}
}

func TestUploadFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/files" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.FormValue("purpose") != "user_data" || r.FormValue("expires_after[seconds]") != "3600" {
			t.Errorf("wrong form: %v", r.MultipartForm)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"file-1","object":"file","bytes":4,"created_at":1767225600,"expires_at":1767229200,
			"filename":"a.pdf","purpose":"user_data","status":"processed"}`)
	}))
	defer server.Close()

	client := New(Config{Model: ModelGPT6Luna, APIKey: "test", BaseURL: server.URL + "/", FileScope: "test"})
	block, err := client.UploadFile(context.Background(), "a.pdf", "application/pdf", strings.NewReader("%PDF"), 3600e9)
	if err != nil {
		t.Fatal(err)
	}
	if block.File == nil || block.File.ID != "file-1" || block.File.Scope != "test" || block.File.ExpiresAt.Unix() != 1767229200 || block.Name != "a.pdf" {
		t.Fatalf("wrong block: %+v %+v", block, block.File)
	}
}
