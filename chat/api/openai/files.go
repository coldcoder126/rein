package openai

import (
	"context"
	"io"
	"time"

	goopenai "github.com/openai/openai-go/v3"

	"github.com/coldcoder126/rein/chat"
)

// UploadFile uploads a file to the Files API with purpose user_data and returns a media block referring to it, ready to put in a message.
// A ttl of 0 keeps the file until it is deleted; otherwise it must be between one hour and 30 days.
// The returned block has no fallback: to keep the session usable on other models, set its URL or data too.
func (c *Client) UploadFile(ctx context.Context, name, mimeType string, r io.Reader, ttl time.Duration) (chat.ContentBlock, error) {
	scope := c.fileScope()
	if scope == "" {
		return chat.ContentBlock{}, &chat.Error{Kind: chat.KindInvalidRequest, Message: "openai: this endpoint has no Files API; set Config.FileScope if it does"}
	}
	client, err := c.newSDKClient(ctx)
	if err != nil {
		return chat.ContentBlock{}, err
	}
	params := goopenai.FileNewParams{File: goopenai.File(r, name, mimeType), Purpose: goopenai.FilePurposeUserData}
	if ttl > 0 {
		params.ExpiresAfter = goopenai.FileNewParamsExpiresAfter{Seconds: int64(ttl / time.Second)}
	}
	file, err := client.Files.New(ctx, params)
	if err != nil {
		return chat.ContentBlock{}, c.wrapError(ctx, err)
	}
	ref := chat.FileRef{Scope: scope, ID: file.ID}
	if file.ExpiresAt > 0 {
		ref.ExpiresAt = time.Unix(file.ExpiresAt, 0)
	}
	return chat.MediaFile(ref, mimeType).WithName(name), nil
}

// DeleteFile deletes an uploaded file. Messages in history that still refer to it fail on the OpenAI side,
// so drop or replace those references first, or keep a fallback on the blocks.
func (c *Client) DeleteFile(ctx context.Context, id string) error {
	client, err := c.newSDKClient(ctx)
	if err != nil {
		return err
	}
	if _, err := client.Files.Delete(ctx, id); err != nil {
		return c.wrapError(ctx, err)
	}
	return nil
}

// fileScope is the Files API store of this client, empty when the endpoint has none.
func (c *Client) fileScope() string {
	switch {
	case c.cfg.FileScope != "":
		return c.cfg.FileScope
	case c.cfg.Endpoint.BaseURL == "":
		return providerName
	}
	return ""
}
