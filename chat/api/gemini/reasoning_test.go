package gemini

import (
	"encoding/json"
	"testing"

	"github.com/coldcoder126/rein/chat"
)

func TestThinkingConfigByModel(t *testing.T) {
	cases := []struct {
		model Model
		level chat.ReasoningLevel
		want  string
	}{
		{flash35, "", `{"includeThoughts":true}`},
		{flash35, chat.ReasoningOff, `{"thinkingBudget":0}`},
		{flash35, chat.ReasoningXHigh, `{"includeThoughts":true,"thinkingLevel":"HIGH"}`},
		{flash35, chat.ReasoningMax, `{"includeThoughts":true,"thinkingLevel":"HIGH"}`},
		{neon4, chat.ReasoningHigh, `{"includeThoughts":true,"thinkingLevel":"HIGH"}`},
		{neon4, chat.ReasoningXHigh, `{"includeThoughts":true,"thinkingLevel":"XHIGH"}`},
		{neon4, chat.ReasoningMax, `{"includeThoughts":true,"thinkingLevel":"XHIGH"}`},
		{flash25, chat.ReasoningOff, `{"thinkingBudget":0}`},
		{flash25, chat.ReasoningLow, `{"includeThoughts":true,"thinkingBudget":2048}`},
	}
	for _, tc := range cases {
		cfg, err := thinkingConfig(tc.model.Compat, tc.level)
		if err != nil {
			t.Fatal(err)
		}
		if raw, _ := json.Marshal(cfg); string(raw) != tc.want {
			t.Fatalf("%s/%s: %s", tc.model.Name, tc.level, raw)
		}
	}
	if _, err := thinkingConfig(Compat{}, "extreme"); err == nil {
		t.Fatal("an unknown reasoning level should fail")
	}
	// 2.x image models do not support thinking: no thinking config by default, and image output is requested.
	_, config, err := mustNew(Config{Model: Model{Name: "gemini-2.5-flash-image", Compat: gemini2}, ImageGeneration: true}).buildRequest(chat.Request{})
	if err != nil || config.ThinkingConfig != nil || len(config.ResponseModalities) != 2 {
		t.Fatalf("wrong image model config: %+v %v", config, err)
	}
}
