package openai

// Endpoint is a service that speaks the OpenAI Responses API: OpenAI itself (the zero value) or a compatible one.
// It states facts about the service; what the caller wants from it is set on Config.
type Endpoint struct {
	// Provider names the vendor in messages, usage and errors; "openai" by default.
	Provider string
	// BaseURL is the address of the API; empty for OpenAI's.
	BaseURL string
	// MaxInputTokens / MaxOutputTokens are the limits of the endpoint's models, used when Config sets none.
	MaxInputTokens  int
	MaxOutputTokens int
	// Compat says where the endpoint differs from OpenAI's.
	Compat Compat
}

// Compat lists what a compatible endpoint lacks compared with OpenAI's own. The zero value lacks nothing.
type Compat struct {
	// NoMedia means the endpoint accepts no media input; media is replaced with a note before the request is sent.
	NoMedia bool
	// NoStoreParams means the endpoint does not recognize the store / include parameters, so they are not sent.
	NoStoreParams bool
	// NoStructuredOutput means the endpoint does not support the json_schema output format; the format requirements
	// go into the system prompt instead.
	NoStructuredOutput bool
	// NoToolSearch means the endpoint has no hosted tool search (it needs gpt-5.4 or later), so deferred tools
	// (chat.ToolDef.Deferred) are sent up front.
	NoToolSearch bool
}

// DeepSeek models, served by the DeepSeek endpoint.
const (
	ModelDeepSeekFlash = "deepseek-flash"
	ModelDeepSeekV4Pro = "deepseek-v4-pro"
)

// DeepSeek is DeepSeek's Responses endpoint. It is stateless and does not recognize store / include; reasoning comes
// back as plain reasoning items that are sent back whole. Media is passed through: set Compat.NoMedia on a copy
// when the model should not receive it.
var DeepSeek = Endpoint{
	Provider:        "deepseek",
	BaseURL:         "https://api.deepseek.com",
	MaxInputTokens:  1_000_000,
	MaxOutputTokens: 384_000,
	Compat:          Compat{NoStoreParams: true, NoStructuredOutput: true, NoToolSearch: true},
}
