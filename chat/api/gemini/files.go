package gemini

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/coldcoder126/rein/chat"
)

// fileStatePoll is how often UploadFile checks whether an uploaded file has finished processing.
const fileStatePoll = time.Second

// UploadFile uploads a file to the Files API and returns a media block referring to it, ready to put in a message; FileRef.ID is the file URI.
// It waits until the file has been processed (video can take a while). The Gemini API deletes files after 48 hours and does not take
// a ttl, so ttl must be 0. The returned block has no fallback: to keep the session usable after that, set its URL or data too.
func (c *Client) UploadFile(ctx context.Context, name, mimeType string, r io.Reader, ttl time.Duration) (chat.ContentBlock, error) {
	scope := c.fileScope()
	switch {
	case scope == "":
		return chat.ContentBlock{}, &chat.Error{Kind: chat.KindInvalidRequest, Message: "gemini: this endpoint has no Files API; set Config.FileScope if it does"}
	case ttl != 0:
		return chat.ContentBlock{}, &chat.Error{Kind: chat.KindInvalidRequest, Message: "gemini: the Files API keeps files for 48 hours and takes no ttl"}
	}
	client, err := c.newSDKClient(ctx)
	if err != nil {
		return chat.ContentBlock{}, err
	}
	file, err := client.Files.Upload(ctx, r, &genai.UploadFileConfig{MIMEType: mimeType, DisplayName: name})
	if err != nil {
		return chat.ContentBlock{}, wrapError(ctx, err)
	}
	for file.State == genai.FileStateProcessing {
		select {
		case <-ctx.Done():
			return chat.ContentBlock{}, wrapError(ctx, ctx.Err())
		case <-time.After(fileStatePoll):
		}
		if file, err = client.Files.Get(ctx, file.Name, nil); err != nil {
			return chat.ContentBlock{}, wrapError(ctx, err)
		}
	}
	if file.State == genai.FileStateFailed {
		msg := "processing failed"
		if file.Error != nil && file.Error.Message != "" {
			msg = file.Error.Message
		}
		return chat.ContentBlock{}, wrapError(ctx, fmt.Errorf("gemini: file %s: %s", file.Name, msg))
	}
	return chat.MediaFile(chat.FileRef{Scope: scope, ID: file.URI, ExpiresAt: file.ExpirationTime}, mimeType).WithName(name), nil
}

// DeleteFile deletes an uploaded file; id is the file URI (FileRef.ID) or the file name (files/...).
func (c *Client) DeleteFile(ctx context.Context, id string) error {
	name := id
	if _, rest, ok := strings.Cut(id, "/files/"); ok {
		name = "files/" + rest
	}
	client, err := c.newSDKClient(ctx)
	if err != nil {
		return err
	}
	if _, err := client.Files.Delete(ctx, name, nil); err != nil {
		return wrapError(ctx, err)
	}
	return nil
}

// fileScope is the Files API store of this client, empty when the endpoint has none.
func (c *Client) fileScope() string {
	switch {
	case c.cfg.FileScope != "":
		return c.cfg.FileScope
	case !c.cfg.Vertex && c.cfg.BaseURL == "":
		return providerName
	}
	return ""
}
