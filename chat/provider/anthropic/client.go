// Package anthropic implements chat.Client with the Anthropic Messages API.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"time"

	goanthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/coldcoder126/rein/chat"
)

const (
	ModelClaudeOpus5_5   = "claude-opus-5-5"
	ModelClaudeSonnet5_5 = "claude-sonnet-5-5"
	ModelClaudeHaiku5_5  = "claude-haiku-5-5"
	ModelClaudeFable5_1  = "claude-fable-5-1"
	ModelClaudeOpus4_7   = "claude-opus-4-7"
	ModelClaudeSonnet4_6 = "claude-sonnet-4-6"
	ModelClaudeHaiku4_5  = "claude-haiku-4-5"

	providerName = "anthropic"

	// API is the protocol this client speaks, see chat.ModelInfo.API.
	API = "anthropic-messages"
)

// APIKeyProvider resolves the API key dynamically, for short-lived tokens.
type APIKeyProvider func(ctx context.Context) (string, error)

// Config configures the Anthropic Client.
type Config struct {
	// Provider names the vendor in messages, usage and errors; "anthropic" by default. Set it for compatible endpoints.
	Provider       string
	APIKey         string
	APIKeyProvider APIKeyProvider
	BaseURL        string
	Model          string
	// MaxInputTokens / MaxOutputTokens of 0 use the model's built-in specs;
	// when a request sets no MaxTokens, MaxOutputTokens is used as max_tokens.
	MaxInputTokens  int
	MaxOutputTokens int
	Timeout         time.Duration
	MaxRetries      int
	Headers         map[string]string
	HTTPClient      *http.Client
	// TextOnly means the endpoint accepts no media input (as with some Anthropic-compatible endpoints).
	TextOnly bool
	// BudgetThinking forces budget thinking (thinking.type=enabled), for compatible endpoints without adaptive thinking;
	// Claude models are configured from built-in specs and need no setting.
	BudgetThinking bool
	// UnsignedThinking means the endpoint's thinking blocks carry no signature and are replayed as is;
	// by default unsigned thinking blocks are downgraded to text, because Anthropic rejects them.
	UnsignedThinking bool
	// ToolSearch picks the server-side tool search used when a request has deferred tools (chat.ToolDef.Deferred):
	// ToolSearchBM25 by default. ToolSearchOff, or a model without tool search, sends every tool up front instead.
	ToolSearch ToolSearch
	// FileScope names the Files API store this key reads, see chat.FileRef.Scope. It defaults to "anthropic" for the
	// official endpoint and to none for a custom BaseURL; set it per workspace when keys of several workspaces are in use.
	FileScope string
	// HTTPHooks are debugging hooks at the raw HTTP layer.
	HTTPHooks chat.HTTPHooks
}

// ToolSearch is the variant of Anthropic's server-side tool search.
type ToolSearch string

const (
	// ToolSearchBM25 searches with natural language queries; it is the default.
	ToolSearchBM25 ToolSearch = "bm25"
	// ToolSearchRegex searches with regular expressions written by the model.
	ToolSearchRegex ToolSearch = "regex"
	// ToolSearchOff disables tool search: deferred tools are sent up front like any other.
	ToolSearchOff ToolSearch = "off"
)

// Client wraps the Anthropic SDK.
type Client struct {
	cfg Config
}

// New creates an Anthropic chat.Client. It panics when cfg.Model is empty.
func New(cfg Config) *Client {
	if cfg.Model == "" {
		panic("anthropic: Config.Model must not be empty")
	}
	if cfg.Provider == "" {
		cfg.Provider = providerName
	}
	return &Client{cfg: cfg}
}

// Stream sends a streaming request. Tool call arguments are assembled inside the provider before they are emitted.
func (c *Client) Stream(ctx context.Context, req chat.Request) iter.Seq[chat.Event] {
	return func(yield func(chat.Event) bool) {
		b := chat.NewBuilder(yield, c.modelInfo(req))

		client, err := c.newSDKClient(ctx)
		if err != nil {
			b.Fail(c.wrapError(ctx, err))
			return
		}
		params, err := c.buildParams(req)
		if err != nil {
			b.Fail(&chat.Error{Kind: chat.KindInvalidRequest, Err: err})
			return
		}
		stream := client.Messages.NewStreaming(ctx, params)
		defer stream.Close()

		var usage goanthropic.Usage
		var stopReason goanthropic.StopReason
		var sawStop bool
		var blockType string
		var toolArgs strings.Builder
		var truncated bool
		// serverRaw is the JSON of the open server tool block, completed with its streamed input at content_block_stop.
		var serverRaw string

		for stream.Next() {
			event := stream.Current()
			switch event.Type {
			case "message_start":
				start := event.AsMessageStart().Message
				usage = start.Usage
				b.SetResponse(start.ID, string(start.Model))
				b.SetUsage(usageFromAnthropic(usage))
				if !b.Start() {
					return
				}
			case "content_block_start":
				cb := event.AsContentBlockStart().ContentBlock
				blockType = cb.Type
				toolArgs.Reset()
				serverRaw = ""
				if isServerToolBlock(blockType) {
					serverRaw = cb.RawJSON()
				}
				switch blockType {
				case "tool_use":
					tu := cb.AsToolUse()
					if !b.ToolCallStart(tu.ID, tu.Name) {
						return
					}
				case "redacted_thinking":
					if !b.RedactedThinking(cb.AsRedactedThinking().Data) {
						return
					}
				}
			case "content_block_delta":
				delta := event.AsContentBlockDelta().Delta
				ok := true
				switch delta.Type {
				case "text_delta":
					ok = b.TextDelta(delta.AsTextDelta().Text)
				case "thinking_delta":
					ok = b.ThinkingDelta(delta.AsThinkingDelta().Thinking)
				case "signature_delta":
					ok = b.ThinkingSignature(delta.AsSignatureDelta().Signature)
				case "input_json_delta":
					toolArgs.WriteString(delta.AsInputJSONDelta().PartialJSON)
				}
				if !ok {
					return
				}
			case "content_block_stop":
				var ok bool
				if serverRaw != "" {
					raw, name, err := serverToolRecord(serverRaw, toolArgs.String())
					if err != nil {
						b.Fail(&chat.Error{Kind: chat.KindUnknown, Message: "anthropic: " + err.Error()})
						return
					}
					ok = b.ServerTool(name, raw)
				} else if blockType == "tool_use" {
					args := json.RawMessage(toolArgs.String())
					if len(args) > 0 && !json.Valid(args) {
						// When the output is truncated the arguments stop halfway: keep the call with empty arguments and mark the finish
						// as length, so upper layers know the call cannot be executed.
						truncated = true
						args = nil
					}
					ok = b.ToolCallEnd(args)
				} else {
					ok = b.EndBlock()
				}
				if !ok {
					return
				}
				blockType = ""
				serverRaw = ""
			case "message_stop":
				sawStop = true
			case "message_delta":
				md := event.AsMessageDelta()
				stopReason = md.Delta.StopReason
				usage = mergeDeltaUsage(usage, md.Usage)
				b.SetUsage(usageFromAnthropic(usage))
			}
		}
		if err := stream.Err(); err != nil {
			b.Fail(c.wrapError(ctx, fmt.Errorf("anthropic stream: %w", err)))
			return
		}
		if !sawStop {
			b.Fail(&chat.Error{Kind: chat.KindNetwork, Message: "anthropic: stream ended before message_stop"})
			return
		}
		if stopReason == goanthropic.StopReasonRefusal {
			b.Fail(&chat.Error{Kind: chat.KindRefusal, Message: "anthropic: the model refused the request"})
			return
		}
		reason := stopReasonFromAnthropic(stopReason)
		if truncated {
			reason = chat.StopReasonLength
		}
		b.Done(reason)
	}
}

// Info returns the model info.
func (c *Client) Info() chat.ModelInfo {
	spec := c.spec(c.cfg.Model)
	return chat.ModelInfo{
		Provider:         c.cfg.Provider,
		API:              API,
		Model:            c.cfg.Model,
		MaxInputTokens:   spec.maxInput,
		MaxOutputTokens:  spec.maxOutput,
		SupportsMedia:    !c.cfg.TextOnly,
		StructuredOutput: spec.structuredOutput,
		FileScope:        c.fileScope(),
		ImageLimits:      chat.DefaultImageLimits,
	}
}

// modelInfo returns the info of the model actually used by this request, for deciding on replay whether it is the same model.
func (c *Client) modelInfo(req chat.Request) chat.ModelInfo {
	info := c.Info()
	info.Model = string(c.resolveModel(req))
	return info
}

func (c *Client) newSDKClient(ctx context.Context) (goanthropic.Client, error) {
	opts := make([]option.RequestOption, 0, 6+len(c.cfg.Headers))
	apiKey := c.cfg.APIKey
	if c.cfg.APIKeyProvider != nil {
		key, err := c.cfg.APIKeyProvider(ctx)
		if err != nil {
			return goanthropic.Client{}, fmt.Errorf("anthropic: resolve API key: %w", err)
		}
		apiKey = key
	}
	opts = append(opts, option.WithAPIKey(apiKey))
	if c.cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.cfg.BaseURL))
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

func (c *Client) resolveModel(req chat.Request) goanthropic.Model {
	if req.Model != "" {
		return goanthropic.Model(req.Model)
	}
	return goanthropic.Model(c.cfg.Model)
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
		ce.Message = c.cfg.Provider
	}
	return ce
}
