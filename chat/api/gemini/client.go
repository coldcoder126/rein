// Package gemini implements chat.Client for Gemini on top of the official google.golang.org/genai SDK.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"google.golang.org/genai"

	"github.com/coldcoder126/rein/chat"
)

const (
	providerName = "gemini"

	// API is the protocol this client speaks, see chat.ModelInfo.API.
	API = "gemini"
)

// APIKeyProvider resolves the API key dynamically, for short-lived tokens.
type APIKeyProvider func(ctx context.Context) (string, error)

// Config configures the Gemini Client.
type Config struct {
	// Endpoint is the service the client talks to: the zero value is Google's Gemini API, and Vertex AI or a
	// compatible endpoint is described by filling one in.
	Endpoint Endpoint
	// Model is the model the client talks to; its Name must be set. Another model is another Client.
	Model          Model
	APIKey         string
	APIKeyProvider APIKeyProvider
	Timeout        time.Duration
	// MaxRetries is how many times a failed request is retried (408, 429 and 5xx); 0 leaves the SDK's default,
	// which is not to retry.
	MaxRetries int
	Headers    map[string]string
	HTTPClient *http.Client

	// ImageGeneration means the model can generate images (such as the gemini-*-image models):
	// requests ask for both TEXT and IMAGE output, and generated images in history are replayed as is.
	ImageGeneration bool
	// FileScope names the Files API store this key reads, see chat.FileRef.Scope. It defaults to "gemini" for the official
	// Gemini API endpoint and to none on Vertex (which has no Files API; use gs:// URLs) or a custom BaseURL.
	FileScope string
	// HTTPHooks are debugging hooks at the raw HTTP layer.
	HTTPHooks chat.HTTPHooks
}

// Client wraps the genai SDK.
type Client struct {
	cfg Config
	// compat is what the endpoint or the model lacks.
	compat Compat
	info   chat.ModelInfo
}

// New creates a Gemini chat.Client. It returns an error when cfg.Model.Name is empty.
func New(cfg Config) (*Client, error) {
	if cfg.Model.Name == "" {
		return nil, errors.New("gemini: Config.Model.Name must not be empty")
	}
	if cfg.Endpoint.Provider == "" {
		cfg.Endpoint.Provider = providerName
	}
	c := &Client{cfg: cfg, compat: cfg.Endpoint.Compat.merge(cfg.Model.Compat)}
	c.info = chat.ModelInfo{
		Provider:         cfg.Endpoint.Provider,
		API:              API,
		Model:            cfg.Model.Name,
		MaxInputTokens:   cfg.Model.MaxInputTokens,
		MaxOutputTokens:  cfg.Model.MaxOutputTokens,
		SupportsMedia:    true,
		StructuredOutput: true,
		ImageOutput:      cfg.ImageGeneration,
		FileScope:        c.fileScope(),
		ImageLimits:      chat.DefaultImageLimits,
	}
	return c, nil
}

// Info returns the model info.
func (c *Client) Info() chat.ModelInfo { return c.info }

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
			BaseURL: c.cfg.Endpoint.BaseURL,
		},
	}
	if c.cfg.Endpoint.Vertex {
		cc.Backend = genai.BackendVertexAI
	} else {
		// genai defaults to v1, where older and preview models lack thinking, function calling and JSON mode.
		cc.HTTPOptions.APIVersion = "v1beta"
	}
	if c.cfg.Timeout > 0 {
		cc.HTTPOptions.Timeout = &c.cfg.Timeout
	}
	if c.cfg.MaxRetries > 0 {
		attempts := int32(c.cfg.MaxRetries + 1)
		cc.HTTPOptions.RetryOptions = &genai.HTTPRetryOptions{Attempts: &attempts}
	}
	if len(c.cfg.Headers) > 0 {
		cc.HTTPOptions.Headers = make(http.Header, len(c.cfg.Headers))
		for k, v := range c.cfg.Headers {
			cc.HTTPOptions.Headers.Set(k, v)
		}
	}
	return genai.NewClient(ctx, cc)
}

func (c *Client) wrapError(ctx context.Context, err error) *chat.Error {
	var status int
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		status = apiErr.Code
	}
	ce := chat.WrapError(ctx, err, status, 0)
	if ce.Message == "" {
		ce.Message = c.cfg.Endpoint.Provider
	}
	return ce
}
