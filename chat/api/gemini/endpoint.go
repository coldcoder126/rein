package gemini

// Endpoint is a service that speaks the Gemini API: Google's Gemini API itself (the zero value), Vertex AI or a
// compatible one. It says where the service is and how to reach it; the model served from it is described by Model.
type Endpoint struct {
	// Provider names the vendor in messages, usage and errors; "gemini" by default.
	Provider string
	// BaseURL is the address of the API; empty for Google's.
	BaseURL string
	// Vertex means the API key belongs to Vertex AI (express mode) and requests go to the Vertex endpoint;
	// by default the Gemini API (generativelanguage.googleapis.com) is used.
	// Full Vertex access based on project, region and credentials is not provided yet.
	Vertex bool
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
	// MaxOutputTokens is the output limit of the model; 0 declares none. It is only reported by Info: a request
	// that sets no MaxTokens sends no limit.
	MaxOutputTokens int
	// Compat says where this model differs from the full protocol; see Compat.
	Compat Compat
}

// Compat lists what is missing compared with the full Gemini API, as its current models speak it. The zero value
// lacks nothing. It is set on an Endpoint for what holds for every model of the service and on a Model for what holds
// for that model only; a Client lacks whatever either of them lacks.
type Compat struct {
	// NoThinkingLevel means only a thinking budget is accepted, not thinkingLevel (Gemini 2.x).
	NoThinkingLevel bool
	// NoXHighThinking means the highest thinking level is HIGH, not XHIGH (every model before Gemini 4 Hydrogen and
	// Neon); ReasoningXHigh and ReasoningMax then ask for HIGH.
	NoXHighThinking bool
	// NoMediaInToolResults means media embedded in function results is not accepted (Gemini 2.x);
	// the media is sent as separate parts of the same message instead.
	NoMediaInToolResults bool
}

// merge returns what is lacking in either c or other.
func (c Compat) merge(other Compat) Compat {
	return Compat{
		NoThinkingLevel:      c.NoThinkingLevel || other.NoThinkingLevel,
		NoXHighThinking:      c.NoXHighThinking || other.NoXHighThinking,
		NoMediaInToolResults: c.NoMediaInToolResults || other.NoMediaInToolResults,
	}
}
