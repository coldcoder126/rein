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
	providerName = "gemini"

	// API is the protocol this client speaks, see chat.ModelInfo.API.
	API = "gemini"
)

// Model is the model a Client talks to. Beyond the zero value only Name must be set: a model is taken to support
// the whole protocol, and one that does not says what it lacks in Compat.
type Model struct {
	// Name is the model name sent to the API.
	Name string
	// MaxInputTokens is the context limit of the model; 0 declares none, and the runner then never compacts history.
	MaxInputTokens int
	// MaxOutputTokens is the output limit of the model; 0 declares none. It is only reported by Info: a request
	// that sets no MaxTokens sends no limit.
	MaxOutputTokens int
	// Compat says where this model differs from the full protocol; see Compat.
	Compat Compat
}

// Compat lists what a model lacks compared with the full Gemini API, as its current models speak it. The zero value
// lacks nothing.
type Compat struct {
	// NoThinkingLevel means the model only accepts a thinking budget, not thinkingLevel (Gemini 2.x).
	NoThinkingLevel bool
	// NoMediaInToolResults means the model does not accept media embedded in function results (Gemini 2.x);
	// the media is sent as separate parts of the same message instead.
	NoMediaInToolResults bool
}

// APIKeyProvider resolves the API key dynamically, for short-lived tokens.
type APIKeyProvider func(ctx context.Context) (string, error)

// Config configures the Gemini Client.
type Config struct {
	APIKey         string
	APIKeyProvider APIKeyProvider
	// Vertex means the API key belongs to Vertex AI (express mode) and requests go to the Vertex endpoint;
	// by default the Gemini API (generativelanguage.googleapis.com) is used.
	// Full Vertex access based on project, region and credentials is not provided yet.
	Vertex  bool
	BaseURL string
	// Model is the model the client talks to; its Name must be set. Another model is another Client.
	Model      Model
	Timeout    time.Duration
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
	cfg  Config
	info chat.ModelInfo
}

// New creates a Gemini chat.Client. It returns an error when cfg.Model.Name is empty.
func New(cfg Config) (*Client, error) {
	if cfg.Model.Name == "" {
		return nil, errors.New("gemini: Config.Model.Name must not be empty")
	}
	c := &Client{cfg: cfg}
	c.info = chat.ModelInfo{
		Provider:         providerName,
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

// Stream sends a streaming request.
func (c *Client) Stream(ctx context.Context, req chat.Request) iter.Seq[chat.Event] {
	return func(yield func(chat.Event) bool) {
		b := chat.NewBuilder(yield, c.info)

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
		for resp, err := range client.Models.GenerateContentStream(ctx, c.cfg.Model.Name, contents, config) {
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
