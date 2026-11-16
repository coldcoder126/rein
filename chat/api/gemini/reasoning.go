package gemini

import (
	"fmt"

	"github.com/coldcoder126/rein/chat"
	"google.golang.org/genai"
)

var thinkingBudgets = map[chat.ReasoningLevel]int32{
	chat.ReasoningLow:    2_048,
	chat.ReasoningMedium: 8_192,
	chat.ReasoningHigh:   16_384,
	chat.ReasoningXHigh:  24_576,
	chat.ReasoningMax:    32_768,
}

var thinkingLevels = map[chat.ReasoningLevel]genai.ThinkingLevel{
	chat.ReasoningLow:    genai.ThinkingLevelLow,
	chat.ReasoningMedium: genai.ThinkingLevelMedium,
	chat.ReasoningHigh:   genai.ThinkingLevelHigh,
	chat.ReasoningXHigh:  genai.ThinkingLevelHigh,
	chat.ReasoningMax:    genai.ThinkingLevelHigh,
}

// thinkingConfig maps the provider-neutral effort to Gemini's thinking config, always asking for thought summaries.
// budgetOnly means the model only accepts a thinking budget, see Compat.NoThinkingLevel.
func thinkingConfig(budgetOnly bool, level chat.ReasoningLevel) (*genai.ThinkingConfig, error) {
	switch level {
	case "":
		return &genai.ThinkingConfig{IncludeThoughts: true}, nil
	case chat.ReasoningOff:
		// A budget of 0 turns thinking off on both 2.x and 3.x.
		return &genai.ThinkingConfig{ThinkingBudget: genai.Ptr[int32](0)}, nil
	}
	if _, ok := thinkingLevels[level]; !ok {
		return nil, fmt.Errorf("gemini: unknown reasoning level %q", level)
	}
	if budgetOnly {
		return &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: genai.Ptr(thinkingBudgets[level])}, nil
	}
	return &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: thinkingLevels[level]}, nil
}
