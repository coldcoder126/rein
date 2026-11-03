// Package gemini implements chat.Client for Gemini on top of the official google.golang.org/genai SDK.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"time"

	"google.golang.org/genai"

	"github.com/coldcoder126/rein/chat"
)

const (
	ModelGemini3_5Flash      = "gemini-3.5-flash"
	ModelGemini3_1ProPreview = "gemini-3.1-pro-preview"
	ModelGemini2_5Flash      = "gemini-2.5-flash"

	providerName = "gemini"

	defaultModel           = ModelGemini3_5Flash
	defaultMaxInputTokens  = 1_048_576
	defaultMaxOutputTokens = 65_536
)

// APIKeyProvider resolves the API key dynamically, for short-lived tokens.
type APIKeyProvider func(ctx context.Context) (string, error)

// Config configures the Gemini Client.
type Config struct {
	APIKey         string
	APIKeyProvider APIKeyProvider
	// Vertex means the API key belongs to Vertex AI (express mode) and requests go to the Vertex endpoint;
	// by default the Gemini API (generativelanguage.googleapis.com) is used.
	// Full Vertex access based on project, region and credentials is not provided yet.
	Vertex          bool
	BaseURL         string
	Model           string
	MaxInputTokens  int
	MaxOutputTokens int
	Timeout         time.Duration
	Headers         map[string]string
	HTTPClient      *http.Client
	// ImageGeneration means the model can generate images (such as the gemini-*-image models):
	// requests ask for both TEXT and IMAGE output, and generated images in history are replayed as is.
	ImageGeneration bool
	// HTTPHooks are debugging hooks at the raw HTTP layer.
	HTTPHooks chat.HTTPHooks
}

// Client wraps the genai SDK.
type Client struct {
	cfg Config
}

// New creates a Gemini chat.Client.
func New(cfg Config) *Client {
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.MaxInputTokens == 0 {
		cfg.MaxInputTokens = defaultMaxInputTokens
	}
	if cfg.MaxOutputTokens == 0 {
		cfg.MaxOutputTokens = defaultMaxOutputTokens
	}
	return &Client{cfg: cfg}
}

// Info returns the model info.
func (c *Client) Info() chat.ModelInfo {
	return chat.ModelInfo{
		Provider:         providerName,
		Model:            c.cfg.Model,
		MaxInputTokens:   c.cfg.MaxInputTokens,
		MaxOutputTokens:  c.cfg.MaxOutputTokens,
		SupportsMedia:    true,
		StructuredOutput: true,
		ImageOutput:      c.cfg.ImageGeneration,
		ImageLimits:      chat.DefaultImageLimits,
	}
}

func (c *Client) resolveModel(req chat.Request) string {
	if req.Model != "" {
		return req.Model
	}
	return c.cfg.Model
}

// Stream sends a streaming request.
func (c *Client) Stream(ctx context.Context, req chat.Request) iter.Seq[chat.Event] {
	return func(yield func(chat.Event) bool) {
		model := c.resolveModel(req)
		b := chat.NewBuilder(yield, providerName, model)

		client, err := c.newSDKClient(ctx)
		if err != nil {
			b.Fail(wrapError(ctx, err))
			return
		}
		contents, config, err := c.buildRequest(req)
		if err != nil {
			b.Fail(&chat.Error{Kind: chat.KindInvalidRequest, Err: err})
			return
		}

		// Cancel the request when iteration stops early, releasing the underlying connection immediately.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		s := &streamState{b: b, last: -1}
		for resp, err := range client.Models.GenerateContentStream(ctx, model, contents, config) {
			if err != nil {
				b.Fail(wrapError(ctx, fmt.Errorf("gemini stream: %w", err)))
				return
			}
			if !s.handle(resp) {
				return
			}
		}
		s.finish()
	}
}

func (c *Client) newSDKClient(ctx context.Context) (*genai.Client, error) {
	apiKey := c.cfg.APIKey
	if c.cfg.APIKeyProvider != nil {
		key, err := c.cfg.APIKeyProvider(ctx)
		if err != nil {
			return nil, fmt.Errorf("gemini: resolve API key: %w", err)
		}
		apiKey = key
	}
	cc := &genai.ClientConfig{
		APIKey:     apiKey,
		Backend:    genai.BackendGeminiAPI,
		HTTPClient: c.cfg.HTTPHooks.HTTPClient(c.cfg.HTTPClient),
		HTTPOptions: genai.HTTPOptions{
			BaseURL: c.cfg.BaseURL,
		},
	}
	if c.cfg.Vertex {
		cc.Backend = genai.BackendVertexAI
	}
	if c.cfg.Timeout > 0 {
		cc.HTTPOptions.Timeout = &c.cfg.Timeout
	}
	if len(c.cfg.Headers) > 0 {
		cc.HTTPOptions.Headers = make(http.Header, len(c.cfg.Headers))
		for k, v := range c.cfg.Headers {
			cc.HTTPOptions.Headers.Set(k, v)
		}
	}
	return genai.NewClient(ctx, cc)
}

func wrapError(ctx context.Context, err error) *chat.Error {
	var status int
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		status = apiErr.Code
	}
	ce := chat.WrapError(ctx, err, status, 0)
	if ce.Message == "" {
		ce.Message = providerName
	}
	return ce
}
