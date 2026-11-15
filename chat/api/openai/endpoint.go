package openai

// Endpoint is a service that speaks the OpenAI Responses API: OpenAI itself (the zero value) or a compatible one.
// It says where the service is; the model served from it is described by Model.
type Endpoint struct {
	// Provider names the vendor in messages, usage and errors; "openai" by default.
	Provider string
	// BaseURL is the address of the API; empty for OpenAI's.
	BaseURL string
	// Compat says where every model of the endpoint differs from the full protocol; see Compat.
	Compat Compat
}

// Model is the model a Client talks to. Beyond the zero value only Name must be set: a model is taken to support
// the whole protocol, and one that does not says what it lacks in Compat.
type Model struct {
	// Name is the model name sent to the API.
	Name string
	// MaxInputTokens is the context limit of the model; 0 declares none, and the runner then never compacts history.
	MaxInputTokens int
	// MaxOutputTokens is the output limit of the model. It is used as max_output_tokens when a request sets no MaxTokens;
	// 0 declares none and the request then sends no limit.
	MaxOutputTokens int
	// Compat says where this model differs from the full protocol; see Compat.
	Compat Compat
}

// Compat lists what is missing compared with the full OpenAI Responses API, as its current models speak it. The zero
// value lacks nothing. It is set on an Endpoint for what holds for every model of the service and on a Model for what
// holds for that model only; a Client lacks whatever either of them lacks.
type Compat struct {
	// NoMedia means media input is not accepted; media is replaced with a note before the request is sent.
	NoMedia bool
	// NoReasoning means the model is not a reasoning model and rejects the reasoning parameters: requests ask for no
	// reasoning.summary and no encrypted reasoning content, and chat.Options.Reasoning is ignored.
	NoReasoning bool
	// NoStoreParams means the store / include parameters are not recognized, so they are not sent.
	NoStoreParams bool
	// NoStructuredOutput means the json_schema output format is not supported; the format requirements go into the
	// system prompt instead.
	NoStructuredOutput bool
	// NoToolSearch means there is no hosted tool search (it needs gpt-5.4 or later), so deferred tools
	// (chat.ToolDef.Deferred) are sent up front.
	NoToolSearch bool
}

// merge returns what is lacking in either c or other.
func (c Compat) merge(other Compat) Compat {
	return Compat{
		NoMedia:            c.NoMedia || other.NoMedia,
		NoReasoning:        c.NoReasoning || other.NoReasoning,
		NoStoreParams:      c.NoStoreParams || other.NoStoreParams,
		NoStructuredOutput: c.NoStructuredOutput || other.NoStructuredOutput,
		NoToolSearch:       c.NoToolSearch || other.NoToolSearch,
	}
}
