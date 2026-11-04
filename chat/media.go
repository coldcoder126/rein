package chat

import (
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Media creates an inline media block: an image, PDF, audio, video, text file or any other content.
// The type is detected from the content; it is application/octet-stream when it cannot be detected. When the detected type
// is not specific enough (an Office document is only detected as zip, for example), give a file name with an extension via WithName, or set the type with WithMIME.
//
// The framework does not restrict, convert or compress the content; it passes it to the provider as is. Whether the provider
// accepts it is for the caller to check; when it does not, the request fails.
func Media(data []byte) ContentBlock {
	return ContentBlock{Type: ContentTypeMedia, Data: data, MIMEType: DetectMIME(data)}
}

// MediaURL creates a media block that references a URL; the provider fetches the content itself. The type is inferred from
// the extension of the URL path, falling back to application/octet-stream; WithMIME sets it explicitly.
//
// The framework neither downloads nor checks the URL: whether it is reachable, whether the content fits the model's size limits and when the link expires are the caller's responsibility.
// History is replayed on every turn, so once the link breaks every request in the session fails.
func MediaURL(rawURL string) ContentBlock {
	block := ContentBlock{Type: ContentTypeMedia, URL: rawURL, MIMEType: genericMIME}
	if u, err := url.Parse(rawURL); err == nil {
		if byExt := mimeByExtension(u.Path); byExt != "" {
			block.MIMEType = byExt
		}
	}
	return block
}

// FileRef refers to a file uploaded to a provider's file store, such as the Anthropic or OpenAI Files API, so requests carry
// an ID instead of the content. Provider clients' UploadFile returns a media block with it filled in.
//
// File IDs are readable by every key of the store (an Anthropic workspace, an OpenAI project): create and keep them on the
// server side and never accept them from end users, or one user can read another's files.
type FileRef struct {
	// Scope names the store the file lives in and must equal the target model's ModelInfo.FileScope for the file to be used.
	Scope string `json:"scope"`
	// ID is the provider's file ID; for Gemini it is the file URI.
	ID string `json:"id"`
	// ExpiresAt is when the store deletes the file, zero for never; shortly before it the file is no longer used.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// MediaFile creates a media block that refers to an uploaded file. mimeType is required: it decides whether the file is sent as an image or a document.
// Without a fallback the block becomes a note when the target model cannot reach the file; prefer WithFile on a block that keeps the URL or data.
func MediaFile(file FileRef, mimeType string) ContentBlock {
	return ContentBlock{Type: ContentTypeMedia, File: &file, MIMEType: mimeType}
}

// WithFile returns the media block referring to an uploaded file; its URL or data stays as the fallback for models that cannot reach the file.
func (b ContentBlock) WithFile(file FileRef) ContentBlock {
	b.File = &file
	return b
}

// WithMIME returns the media block with the given MIME type.
func (b ContentBlock) WithMIME(mimeType string) ContentBlock {
	b.MIMEType = mimeType
	return b
}

// WithName returns the media block with a file name. When the type is not detected yet or not specific enough, it is completed from the file extension.
func (b ContentBlock) WithName(name string) ContentBlock {
	b.Name = name
	if isGenericMIME(b.MIMEType) {
		if byExt := mimeByExtension(name); byExt != "" {
			b.MIMEType = byExt
		}
	}
	return b
}

// IsMedia reports whether the block is a media block.
func (b ContentBlock) IsMedia() bool { return b.Type == ContentTypeMedia }

// IsImage reports whether the block is an image.
func (b ContentBlock) IsImage() bool { return b.isMediaOf("image/") }

// IsAudio reports whether the block is audio.
func (b ContentBlock) IsAudio() bool { return b.isMediaOf("audio/") }

// IsVideo reports whether the block is video.
func (b ContentBlock) IsVideo() bool { return b.isMediaOf("video/") }

func (b ContentBlock) isMediaOf(prefix string) bool {
	return b.Type == ContentTypeMedia && strings.HasPrefix(b.MIMEType, prefix)
}

// genericMIME is the MIME type used when the type cannot be detected.
const genericMIME = "application/octet-stream"

// DetectMIME detects the MIME type from the content, returning application/octet-stream when it cannot be detected.
func DetectMIME(data []byte) string {
	mimeType, _, _ := strings.Cut(http.DetectContentType(data), ";")
	return mimeType
}

// isGenericMIME reports whether a type detected from content still needs refining by extension:
// plain text may be Markdown, CSV or code, and zip may be an Office document.
func isGenericMIME(mimeType string) bool {
	switch mimeType {
	case "", genericMIME, "application/zip", "text/plain":
		return true
	}
	return false
}

func mimeByExtension(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return ""
	}
	// MIME tables differ across systems, so common text extensions are pinned here.
	switch ext {
	case ".md", ".markdown":
		return "text/markdown"
	case ".csv":
		return "text/csv"
	case ".webp":
		return "image/webp"
	}
	mimeType, _, _ := strings.Cut(mime.TypeByExtension(ext), ";")
	return mimeType
}

// mediaPlaceholder returns the note that replaces a media block where the endpoint does not support media input.
func mediaPlaceholder(b ContentBlock) ContentBlock {
	return Text(fmt.Sprintf("(attachment omitted: this model does not accept media input: %s)", mediaDescription(b)))
}

// fileExpiryMargin is how long before FileRef.ExpiresAt a file is no longer used, so a request does not race the deletion.
const fileExpiryMargin = 5 * time.Minute

// resolveFile keeps the file reference of a media block only when the target scope can still read the file; otherwise it falls back
// to the block's data or URL, or, with neither, to a note.
func resolveFile(b ContentBlock, scope string, now time.Time) ContentBlock {
	f := b.File
	if f == nil || (f.Scope != "" && f.Scope == scope && (f.ExpiresAt.IsZero() || now.Add(fileExpiryMargin).Before(f.ExpiresAt))) {
		return b
	}
	b.File = nil
	if len(b.Data) > 0 || b.URL != "" {
		return b
	}
	return Text(fmt.Sprintf("(attachment omitted: the file is not available to this model: %s)", mediaDescription(b)))
}

func mediaDescription(b ContentBlock) string {
	if b.Name != "" {
		return b.Name + ", " + b.MIMEType
	}
	return b.MIMEType
}
