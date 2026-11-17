package anthropic

// Endpoint is a service that speaks the Anthropic Messages API: Anthropic itself (the zero value) or a compatible one.
// It says where the service is and how to reach it; the model served from it is described by Model.
type Endpoint struct {
	// Provider names the vendor in messages, usage and errors; "anthropic" by default.
	Provider string
	// BaseURL is the address of the API; empty for Anthropic's.
	BaseURL string
	// Auth is how the API key is sent; AuthAPIKey by default.
	Auth Auth
	// Compat says where every model of the endpoint differs from the full protocol; see Compat.
	Compat Compat
}

// Auth is the way an endpoint takes the API key.
type Auth string

const (
	// AuthAPIKey sends the key in the x-api-key header, as Anthropic does; it is the default.
	AuthAPIKey Auth = ""
	// AuthBearer sends the key as "Authorization: Bearer <key>".
	AuthBearer Auth = "bearer"
)

// Model is the model a Client talks to. Beyond the zero value only Name must be set: a model is taken to support
// the whole protocol, and one that does not says what it lacks in Compat.
type Model struct {
	// Name is the model name sent to the API.
	Name string
	// MaxInputTokens is the context limit of the model; 0 declares none, and the runner then never compacts history.
	MaxInputTokens int
	// MaxOutputTokens is the output limit of the model. It is used as max_tokens when a request sets no MaxTokens;
	// 0 declares none and DefaultMaxTokens is used instead.
	MaxOutputTokens int
	// Compat says where this model differs from the full protocol; see Compat.
	Compat Compat
}

// DefaultMaxTokens is the max_tokens of a request that sets no MaxTokens to a model that declares no MaxOutputTokens.
// The protocol requires the field, and the framework does not know the limits of models.
const DefaultMaxTokens = 32_000

// Compat lists what is missing compared with the full Anthropic Messages API, as its current models speak it. The zero
// value lacks nothing. It is set on an Endpoint for what holds for every model of the service and on a Model for what
// holds for that model only; a Client lacks whatever either of them lacks.
type Compat struct {
	// NoMedia means media input is not accepted; media is replaced with a note before the request is sent.
	NoMedia bool
	// NoAdaptiveThinking means only budget thinking is known (thinking.type=enabled with budget_tokens), not
	// adaptive thinking with output_config.effort.
	NoAdaptiveThinking bool
	// NoDisableThinking means thinking cannot be turned off: chat.ReasoningOff asks for the lowest effort instead,
	// and forced tool calls, which do not go with thinking, are rejected before the request is sent.
	NoDisableThinking bool
	// NoXHigh means effort has no xhigh; high is used instead.
	NoXHigh bool
	// NoMaxEffort means effort has no max; xhigh is used instead, or high with NoXHigh.
	NoMaxEffort bool
	// NoThinkingBinding means thinking.block_binding and its beta header are not accepted, so Config.PrefixMismatch
	// is not sent and the service's own default applies.
	NoThinkingBinding bool
	// NoThinkingSignature means thinking blocks carry no signature and are replayed as they are; otherwise unsigned
	// thinking blocks are replayed as text, because Anthropic rejects them.
	NoThinkingSignature bool
	// NoStructuredOutput means output_config.format is not supported; the format requirements go into the system
	// prompt instead.
	NoStructuredOutput bool
	// NoToolSearch means there is no server-side tool search, so deferred tools (chat.ToolDef.Deferred) are sent up front.
	NoToolSearch bool
}

// merge returns what is lacking in either c or other.
func (c Compat) merge(other Compat) Compat {
	return Compat{
		NoMedia:             c.NoMedia || other.NoMedia,
		NoAdaptiveThinking:  c.NoAdaptiveThinking || other.NoAdaptiveThinking,
		NoDisableThinking:   c.NoDisableThinking || other.NoDisableThinking,
		NoXHigh:             c.NoXHigh || other.NoXHigh,
		NoMaxEffort:         c.NoMaxEffort || other.NoMaxEffort,
		NoThinkingBinding:   c.NoThinkingBinding || other.NoThinkingBinding,
		NoThinkingSignature: c.NoThinkingSignature || other.NoThinkingSignature,
		NoStructuredOutput:  c.NoStructuredOutput || other.NoStructuredOutput,
		NoToolSearch:        c.NoToolSearch || other.NoToolSearch,
	}
}
