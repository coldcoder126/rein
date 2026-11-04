package chat

import (
	"context"
	"iter"
)

// Usage holds the standard token usage of one model call.
type Usage struct {
	Input       int     `json:"input"`
	Output      int     `json:"output"`
	CacheRead   int     `json:"cache_read"`
	CacheWrite  int     `json:"cache_write"`
	TotalTokens int     `json:"total_tokens"`
	Cost        float64 `json:"cost,omitempty"`
}

// Add returns the sum of two Usage values.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		Input:       u.Input + other.Input,
		Output:      u.Output + other.Output,
		CacheRead:   u.CacheRead + other.CacheRead,
		CacheWrite:  u.CacheWrite + other.CacheWrite,
		TotalTokens: u.TotalTokens + other.TotalTokens,
		Cost:        u.Cost + other.Cost,
	}
}

// ModelInfo describes the runtime configuration of a model.
type ModelInfo struct {
	// Provider is the vendor serving the model (anthropic, deepseek...), for display, usage and errors.
	Provider string
	// API is the protocol of the endpoint (anthropic-messages, openai-responses...). Replies keep provider-private data
	// (signatures, item IDs) that only the same provider, API and model can replay.
	API             string
	Model           string
	MaxInputTokens  int
	MaxOutputTokens int
	// SupportsMedia reports whether the endpoint accepts media input. When false (a text-only endpoint), media blocks in user
	// messages and tool results are replaced with a note before the request is sent. It does not distinguish media types: whether a type is accepted is up to the provider.
	SupportsMedia bool
	// ImageOutput reports whether model-generated images in assistant messages can be replayed to this model;
	// when false, replaying history replaces those images with placeholder text.
	ImageOutput bool
	// StructuredOutput reports whether the model natively supports output constrained by a JSON Schema;
	// when false, the provider writes the format requirements into the system prompt.
	StructuredOutput bool
	// FileScope names the file store this endpoint reads uploaded files from (see FileRef.Scope), empty when it reads none.
	// Media blocks whose file belongs to another scope fall back to their data or URL on replay.
	FileScope string
	// ImageLimits are the image specs the model accepts, for callers that process images with imageproc; zero fields use the defaults.
	ImageLimits ImageLimits
}

// ImageLimits describe the image specs a model accepts.
type ImageLimits struct {
	MaxWidth  int
	MaxHeight int
	// MaxBytes limits the size after base64 encoding.
	MaxBytes    int
	JPEGQuality int
}

// DefaultImageLimits are conservative defaults: at most 2000 pixels on the longest side and at most 4.5MB of base64 (below Anthropic's 5MB limit).
var DefaultImageLimits = ImageLimits{
	MaxWidth:    2000,
	MaxHeight:   2000,
	MaxBytes:    4_718_592,
	JPEGQuality: 80,
}

// WithDefaults fills zero fields from DefaultImageLimits.
func (l ImageLimits) WithDefaults() ImageLimits {
	if l.MaxWidth <= 0 {
		l.MaxWidth = DefaultImageLimits.MaxWidth
	}
	if l.MaxHeight <= 0 {
		l.MaxHeight = DefaultImageLimits.MaxHeight
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultImageLimits.MaxBytes
	}
	if l.JPEGQuality <= 0 {
		l.JPEGQuality = DefaultImageLimits.JPEGQuality
	}
	return l
}

// ReasoningLevel is a provider-neutral thinking effort, mapped by each provider to its own parameter
// (Anthropic's adaptive thinking effort or thinking budget, OpenAI's reasoning.effort).
type ReasoningLevel string

const (
	// ReasoningOff turns thinking off; models that cannot turn it off use the lowest effort.
	ReasoningOff    ReasoningLevel = "off"
	ReasoningLow    ReasoningLevel = "low"
	ReasoningMedium ReasoningLevel = "medium"
	ReasoningHigh   ReasoningLevel = "high"
	// ReasoningXHigh falls back to high when the model does not support it.
	ReasoningXHigh ReasoningLevel = "xhigh"
)

// CacheRetention is how long a prompt cache entry is kept.
type CacheRetention string

const (
	CacheRetentionNone CacheRetention = "none"
	// CacheRetentionShort is the default (5 minutes on Anthropic).
	CacheRetentionShort CacheRetention = "short"
	// CacheRetentionLong uses the provider's long retention (1 hour on Anthropic, with more expensive writes).
	CacheRetentionLong CacheRetention = "long"
)

// ToolChoiceMode controls whether and how the model calls tools.
type ToolChoiceMode string

const (
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceNone forbids tool calls.
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceRequired requires at least one tool call.
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceTool requires a call to the tool named by ToolChoice.Name.
	ToolChoiceTool ToolChoiceMode = "tool"
)

// ToolChoice constrains tool calls; the zero value uses the model's default behavior (auto). It is ignored when the request has no tools.
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name is only used when Mode is ToolChoiceTool.
	Name string
}

// ForceTool returns a ToolChoice that requires a call to the named tool.
func ForceTool(name string) ToolChoice { return ToolChoice{Mode: ToolChoiceTool, Name: name} }

// Options are the model parameters of one request. The agent passes them through unchanged, so new parameters need no changes in upper layers.
type Options struct {
	// Model defaults to the Client's model when empty.
	Model     string
	MaxTokens int
	// Temperature uses the model default when nil; it can be set to 0 explicitly.
	Temperature *float64
	// Output, when set, requires output that follows a JSON Schema; see OutputFormat.
	Output *OutputFormat
	// ToolChoice constrains tool calls; see ToolChoice.
	ToolChoice ToolChoice
	// Reasoning uses the model's default behavior when empty.
	Reasoning ReasoningLevel
	// CacheRetention defaults to CacheRetentionShort when empty; providers without explicit caching ignore it.
	CacheRetention CacheRetention
	Metadata       map[string]string
}

// Float returns a pointer to v, for setting Options.Temperature.
func Float(v float64) *float64 { return &v }

// Request is a chat request.
type Request struct {
	Options
	SystemPrompt string
	Messages     []Message
	Tools        []ToolDef
}

// Client is the lowest-level chat client interface. It neither executes tools nor stores history.
//
// The event sequence returned by Stream honors this contract:
//   - it starts with EventStart and ends with exactly one EventDone or EventError;
//   - text, thinking and tool call blocks come as paired Start/Delta/End events, and Index is the block's position in Message.Content;
//     image blocks have no deltas: a single EventImage gives the complete image;
//   - every failure (including cancellation) is reported through EventError, whose Message keeps the content generated so far;
//     on failure a tool call block that is still open has incomplete arguments and is dropped (no end event);
//   - Event.Message is a live object that advances with the events and is only valid during the callback; call Clone to keep it;
//   - when the caller stops iterating early, the implementation should release the underlying connection immediately.
type Client interface {
	Stream(ctx context.Context, req Request) iter.Seq[Event]
	Info() ModelInfo
}
