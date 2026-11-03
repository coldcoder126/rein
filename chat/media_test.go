package chat_test

import (
	"testing"

	"github.com/coldcoder126/rein/chat"
)

func TestMediaDetectsType(t *testing.T) {
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
	cases := []struct {
		name  string
		block chat.ContentBlock
		want  string
	}{
		{"webp content", chat.Media(webp), "image/webp"},
		{"pdf content", chat.Media([]byte("%PDF-1.7")), "application/pdf"},
		{"text content", chat.Media([]byte("hello")), "text/plain"},
		{"text refined by file name", chat.Media([]byte("# 标题")).WithName("说明.md"), "text/markdown"},
		{"zip refined by file name", chat.Media([]byte("PK\x03\x04")).WithName("a.docx"), "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"specific content not overridden by file name", chat.Media([]byte("%PDF-1.7")).WithName("假装.png"), "application/pdf"},
		{"undetectable", chat.Media([]byte{0, 1, 2, 3}), "application/octet-stream"},
		{"undetectable falls back to file name", chat.Media([]byte{0, 1, 2, 3}).WithName("录音.mp3"), "audio/mpeg"},
		{"URL extension", chat.MediaURL("https://cdn.example.com/x/a.WEBP?token=1"), "image/webp"},
		{"URL without extension", chat.MediaURL("https://example.com/download?id=1"), "application/octet-stream"},
		{"explicit type", chat.MediaURL("https://example.com/download?id=1").WithMIME("audio/mpeg"), "audio/mpeg"},
	}
	for _, c := range cases {
		if c.block.Type != chat.ContentTypeMedia || c.block.MIMEType != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.block.MIMEType, c.want)
		}
	}
	if b := chat.Media(webp); !b.IsImage() || b.IsAudio() || b.IsVideo() || chat.Text("x").IsImage() {
		t.Error("wrong type predicates")
	}
}
