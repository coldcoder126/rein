package gemini

import (
	"fmt"
	"slices"

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
	chat.ReasoningXHigh:  genai.ThinkingLevelXhigh,
	chat.ReasoningMax:    genai.ThinkingLevelXhigh,
}

// thinkingOrder lists the thinking levels from lowest to highest.
var thinkingOrder = []genai.ThinkingLevel{
	genai.ThinkingLevelMinimal, genai.ThinkingLevelLow, genai.ThinkingLevelMedium, genai.ThinkingLevelHigh, genai.ThinkingLevelXhigh,
}

// lacks reports whether the model rejects the thinking level.
func (c Compat) lacks(level genai.ThinkingLevel) bool {
	switch level {
	case genai.ThinkingLevelLow:
		return c.NoLowThinking
	case genai.ThinkingLevelMedium:
		return c.NoMediumThinking
	case genai.ThinkingLevelXhigh:
		return c.NoXHighThinking
	}
	return false
}

// thinkingConfig maps the provider-neutral effort to Gemini's thinking config, always asking for thought summaries.
// The levels the model lacks are given by compat: a budget instead of a level, and a level it rejects falls to the
// next lower one.
func thinkingConfig(compat Compat, level chat.ReasoningLevel) (*genai.ThinkingConfig, error) {
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
	if compat.NoThinkingLevel {
		return &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: genai.Ptr(thinkingBudgets[level])}, nil
	}
	i := slices.Index(thinkingOrder, thinkingLevels[level])
	for i > 0 && compat.lacks(thinkingOrder[i]) {
		i--
	}
	return &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: thinkingOrder[i]}, nil
}
