package anthropic

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

func TestBuildParamsReasoning(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		opts    chat.Options
		want    []string
		notWant []string
	}{
		{"default thinking is stated to carry the block binding", Config{Model: sonnet55}, chat.Options{},
			[]string{`"thinking":{"type":"adaptive",` + `"block_binding":{"prefix_mismatch_behavior":"drop_block"}` + `}`}, []string{`"output_config"`}},
		{"no thinking parameters with the binding off", Config{Model: sonnet55, Endpoint: Endpoint{Compat: Compat{NoThinkingBinding: true}}}, chat.Options{}, nil, []string{`"thinking"`, `"output_config"`}},
		{"the binding can reject instead", Config{Model: opus55, PrefixMismatch: PrefixMismatchError}, chat.Options{},
			[]string{`"prefix_mismatch_behavior":"error"`}, nil},
		{"adaptive models use effort", Config{Model: sonnet55}, chat.Options{Reasoning: chat.ReasoningXHigh, Temperature: chat.Float(0.2)},
			[]string{`"thinking":{"display":"summarized","type":"adaptive",` + `"block_binding":{"prefix_mismatch_behavior":"drop_block"}` + `}`, `"effort":"xhigh"`}, []string{`"temperature"`}},
		{"fall back to high without xhigh", Config{Model: sonnet46}, chat.Options{Reasoning: chat.ReasoningXHigh},
			[]string{`"type":"adaptive"`, `"effort":"high"`}, []string{`"block_binding"`}},
		{"models that cannot turn thinking off use the lowest effort", Config{Model: sonnet55}, chat.Options{Reasoning: chat.ReasoningOff},
			[]string{`"effort":"low"`, `"block_binding":{"prefix_mismatch_behavior":"drop_block"}`}, nil},
		{"models that can turn it off send disabled", Config{Model: haiku55}, chat.Options{Reasoning: chat.ReasoningOff, Temperature: chat.Float(0.2)},
			[]string{`"thinking":{"type":"disabled"}`, `"temperature":0.2`}, []string{`"output_config"`, `"block_binding"`}},
		{"older models use a budget and raise max_tokens", Config{Model: haiku45}, chat.Options{Reasoning: chat.ReasoningMedium, MaxTokens: 1000},
			[]string{`"budget_tokens":8192`, `"type":"enabled"`, `"max_tokens":9192`}, []string{`"output_config"`, `"block_binding"`}},
		{"budget within the model output limit", Config{Model: Model{Name: "m", MaxOutputTokens: 4096, Compat: Compat{NoAdaptiveThinking: true}}}, chat.Options{Reasoning: chat.ReasoningHigh},
			[]string{`"budget_tokens":3072`, `"max_tokens":4096`}, nil},
		{"an explicit max_tokens grows by the budget up to the model output limit", Config{Model: Model{Name: "m", MaxOutputTokens: 4096, Compat: Compat{NoAdaptiveThinking: true}}}, chat.Options{Reasoning: chat.ReasoningHigh, MaxTokens: 3000},
			[]string{`"budget_tokens":3072`, `"max_tokens":4096`}, nil},
		{"max is the highest effort", Config{Model: haiku55}, chat.Options{Reasoning: chat.ReasoningMax},
			[]string{`"effort":"max"`}, nil},
		{"fall back from max to xhigh", Config{Model: Model{Name: "m", Compat: Compat{NoMaxEffort: true}}}, chat.Options{Reasoning: chat.ReasoningMax},
			[]string{`"effort":"xhigh"`}, nil},
		{"fall back from max to high without xhigh", Config{Model: sonnet46}, chat.Options{Reasoning: chat.ReasoningMax},
			[]string{`"effort":"high"`}, nil},
		{"budget models that cannot turn thinking off use the smallest budget", Config{Model: Model{Name: "m", Compat: Compat{NoAdaptiveThinking: true, NoDisableThinking: true}}}, chat.Options{Reasoning: chat.ReasoningOff},
			[]string{`"budget_tokens":1024`, `"type":"enabled"`}, []string{`"output_config"`}},
		{"a model that declares no output limit uses the default max_tokens", Config{Model: Model{Name: "m"}}, chat.Options{},
			[]string{`"max_tokens":32000`}, nil},
		{"the model output limit is the default max_tokens", Config{Model: haiku45}, chat.Options{},
			[]string{`"max_tokens":64000`}, nil},
		{"models without adaptive thinking use a budget", Config{Model: thirdParty}, chat.Options{Reasoning: chat.ReasoningLow},
			[]string{`"budget_tokens":2048`}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := paramsJSON(t, mustNew(tc.cfg), chat.Request{Options: tc.opts})
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %s: %s", want, got)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Fatalf("should not contain %s: %s", notWant, got)
				}
			}
		})
	}
	if _, err := mustNew(Config{Model: sonnet55}).buildParams(chat.Request{Options: chat.Options{Reasoning: "extreme"}}); err == nil {
		t.Fatal("an unknown reasoning level should fail")
	}
}

func TestStreamSendsTheBlockBindingBeta(t *testing.T) {
	cases := []struct {
		model Model
		cfg   Config
		want  []string
	}{
		{sonnet55, Config{Headers: map[string]string{"anthropic-beta": "other-beta"}}, []string{"other-beta", prefixMismatchBeta}},
		{sonnet55, Config{Endpoint: Endpoint{Compat: Compat{NoThinkingBinding: true}}}, nil},
		{sonnet46, Config{}, nil},
	}
	for _, tc := range cases {
		var got []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Values("anthropic-beta")
			serveSSE(sse(ev("message_start", messageStart), ev("message_stop", `{"type":"message_stop"}`)))(w, r)
		}))
		cfg := tc.cfg
		cfg.APIKey, cfg.Endpoint.BaseURL, cfg.Model = "test", server.URL, tc.model
		for range mustNew(cfg).Stream(t.Context(), chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("hi")}}) {
		}
		server.Close()
		if !slices.Equal(got, tc.want) {
			t.Fatalf("%s %+v: anthropic-beta = %v, want %v", tc.model.Name, tc.cfg, got, tc.want)
		}
	}
}
