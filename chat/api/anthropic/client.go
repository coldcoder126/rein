// Package anthropic implements chat.Client with the Anthropic Messages API.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	goanthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/coldcoder126/rein/chat"
)

const (
	providerName = "anthropic"

	// API is the protocol this client speaks, see chat.ModelInfo.API.
	API = "anthropic-messages"
)

// APIKeyProvider resolves the API key dynamically, for short-lived tokens.
type APIKeyProvider func(ctx context.Context) (string, error)

// Config configures the Anthropic Client.
type Config struct {
	// Endpoint is the service the client talks to: the zero value is Anthropic's own, and a compatible
	// endpoint is described by filling one in.
	Endpoint Endpoint
	// Model is the model the client talks to; its Name must be set. Another model is another Client.
	Model          Model
	APIKey         string
	APIKeyProvider APIKeyProvider
	Timeout        time.Duration
	MaxRetries     int
	Headers        map[string]string
	HTTPClient     *http.Client
	// ToolSearch picks the server-side tool search used when a request has deferred tools (chat.ToolDef.Deferred):
	// ToolSearchRegex by default. ToolSearchOff, or Compat.NoToolSearch, sends every tool up front instead.
	ToolSearch ToolSearch
	// PrefixMismatch says what the API does with a replayed thinking block whose conversation changed since it was produced,
	// on models that bind thinking to the conversation: PrefixMismatchDrop by default. Endpoints and models that do not
	// accept the setting say so with Compat.NoThinkingBinding.
	PrefixMismatch PrefixMismatch
	// ExtraBody are top-level fields added to the body of every Messages request, for what a service accepts outside
	// the protocol. A field named like one the client builds replaces it whole.
	ExtraBody map[string]any
	// ResponseFields names top-level fields of the response message that are outside the protocol and worth keeping;
	// those the service sends are kept as raw JSON in chat.Message.Extra.
	ResponseFields []string
	// FileScope names the Files API store this key reads, see chat.FileRef.Scope. It defaults to "anthropic" for the
	// official endpoint and to none for a custom BaseURL; set it per workspace when keys of several workspaces are in use.
	FileScope string
	// HTTPHooks are debugging hooks at the raw HTTP layer.
	HTTPHooks chat.HTTPHooks
}

// ToolSearch is the variant of Anthropic's server-side tool search.
type ToolSearch string

const (
	// ToolSearchRegex searches with regular expressions written by the model; it is the default. It matches substrings,
	// so it finds tools described in Chinese and tools sharing a name prefix such as mcp__{server}__.
	ToolSearchRegex ToolSearch = "regex"
	// ToolSearchBM25 searches with natural language queries. It splits words on spaces, so in testing it did not find
	// tools described in Chinese.
	ToolSearchBM25 ToolSearch = "bm25"
	// ToolSearchOff disables tool search: deferred tools are sent up front like any other.
	ToolSearchOff ToolSearch = "off"
)

// PrefixMismatch is the handling of a replayed thinking block that fails the API's conversation check: the system prompt,
// tools or earlier messages changed since the block was produced, for example after compaction or when run-scoped context
// is left out of later runs. Enforced accounts reject such a request by default.
type PrefixMismatch string

const (
	// PrefixMismatchDrop has the API drop the failing blocks and answer without that reasoning; it is the default.
	// In testing only the stale blocks were dropped: those produced after the change stayed valid.
	PrefixMismatchDrop PrefixMismatch = "drop_block"
	// PrefixMismatchError rejects the request, for finding where history is edited.
	PrefixMismatchError PrefixMismatch = "error"
)

// prefixMismatchBeta is the beta that allows setting thinking.block_binding.
const prefixMismatchBeta = "thinking-binding-controls-2026-08-01"

// Client wraps the Anthropic SDK.
type Client struct {
	cfg Config
	// compat is what the endpoint or the model lacks.
	compat Compat
	info   chat.ModelInfo
}

// New creates an Anthropic chat.Client. It returns an error when cfg.Model.Name is empty or cfg.Endpoint.Auth is unknown.
func New(cfg Config) (*Client, error) {
	if cfg.Model.Name == "" {
		return nil, errors.New("anthropic: Config.Model.Name must not be empty")
	}
	if cfg.Endpoint.Auth != AuthAPIKey && cfg.Endpoint.Auth != AuthBearer {
		return nil, fmt.Errorf("anthropic: unknown Endpoint.Auth %q", cfg.Endpoint.Auth)
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
		SupportsMedia:    !c.compat.NoMedia,
		StructuredOutput: !c.compat.NoStructuredOutput,
		FileScope:        c.fileScope(),
		ImageLimits:      chat.DefaultImageLimits,
	}
	return c, nil
}

// Info returns the model info.
func (c *Client) Info() chat.ModelInfo { return c.info }

func (c *Client) newSDKClient(ctx context.Context) (goanthropic.Client, error) {
	opts := make([]option.RequestOption, 0, 7+len(c.cfg.Headers))
	apiKey := c.cfg.APIKey
	if c.cfg.APIKeyProvider != nil {
		key, err := c.cfg.APIKeyProvider(ctx)
		if err != nil {
			return goanthropic.Client{}, fmt.Errorf("anthropic: resolve API key: %w", err)
		}
		apiKey = key
	}
	// The SDK also reads credentials from the environment; the header of the other scheme is removed so that a key
	// found there is never sent along to this endpoint.
	if c.cfg.Endpoint.Auth == AuthBearer {
		opts = append(opts, option.WithHeaderDel("X-Api-Key"), option.WithAuthToken(apiKey))
	} else {
		opts = append(opts, option.WithHeaderDel("Authorization"), option.WithAPIKey(apiKey))
	}
	if c.cfg.Endpoint.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.cfg.Endpoint.BaseURL))
	}
	if hc := c.cfg.HTTPHooks.HTTPClient(c.cfg.HTTPClient); hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	if c.cfg.Timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(c.cfg.Timeout))
	}
	if c.cfg.MaxRetries > 0 {
		opts = append(opts, option.WithMaxRetries(c.cfg.MaxRetries))
	}
	for k, v := range c.cfg.Headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	return goanthropic.NewClient(opts...), nil
}

func (c *Client) wrapError(ctx context.Context, err error) *chat.Error {
	var status int
	var retryAfter time.Duration
	var apiErr *goanthropic.Error
	if errors.As(err, &apiErr) {
		status = apiErr.StatusCode
		if apiErr.Response != nil {
			retryAfter = chat.ParseRetryAfter(apiErr.Response.Header)
		}
	}
	ce := chat.WrapError(ctx, err, status, retryAfter)
	if ce.Message == "" {
		ce.Message = c.cfg.Endpoint.Provider
	}
	return ce
}
