package anthropic

import (
	goanthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/coldcoder126/rein/chat"
)

const (
	minThinkingBudget = 1024
	// minAnswerTokens is the budget always reserved for the answer when thinking and the answer share max_tokens.
	minAnswerTokens = 1024
)

var thinkingBudgets = map[chat.ReasoningLevel]int64{
	chat.ReasoningLow:    2_048,
	chat.ReasoningMedium: 8_192,
	chat.ReasoningHigh:   16_384,
	chat.ReasoningXHigh:  32_768,
	chat.ReasoningMax:    65_536,
}

// applyReasoning sets thinking / output_config from the provider-neutral effort and reports whether thinking is on for this request.
// maxOutput is the model's output limit, 0 when it declares none. maxTokensExplicit means max_tokens is the answer limit set by the
// caller: budget thinking shares max_tokens with the answer, so the budget is added to the limit to keep thinking from crowding out the answer.
func applyReasoning(params *goanthropic.MessageNewParams, level chat.ReasoningLevel, compat Compat, maxOutput int, maxTokensExplicit bool) (bool, error) {
	if level == "" {
		return false, nil
	}
	budget, ok := thinkingBudgets[level]
	switch {
	case level == chat.ReasoningOff && !compat.NoDisableThinking:
		params.Thinking = goanthropic.ThinkingConfigParamUnion{OfDisabled: &goanthropic.ThinkingConfigDisabledParam{}}
		return false, nil
	case level == chat.ReasoningOff:
		// Thinking stays on at the lowest effort.
		level, budget = chat.ReasoningLow, minThinkingBudget
	case !ok:
		return false, &chat.Error{Kind: chat.KindInvalidRequest, Message: "anthropic: unknown reasoning level " + string(level)}
	}

	if !compat.NoAdaptiveThinking {
		if level == chat.ReasoningMax && compat.NoMaxEffort {
			level = chat.ReasoningXHigh
		}
		if level == chat.ReasoningXHigh && compat.NoXHigh {
			level = chat.ReasoningHigh
		}
		params.Thinking = goanthropic.ThinkingConfigParamUnion{OfAdaptive: &goanthropic.ThinkingConfigAdaptiveParam{
			Display: goanthropic.ThinkingConfigAdaptiveDisplaySummarized,
		}}
		params.OutputConfig.Effort = goanthropic.OutputConfigEffort(level)
		return true, nil
	}

	answer := int64(minAnswerTokens)
	if maxTokensExplicit {
		answer = min(answer, params.MaxTokens)
		params.MaxTokens += budget
		if maxOutput > 0 {
			params.MaxTokens = min(params.MaxTokens, int64(maxOutput))
		}
	}
	budget = min(budget, params.MaxTokens-answer)
	if budget < minThinkingBudget {
		// The output limit is too small for the minimum thinking budget.
		return false, nil
	}
	params.Thinking = goanthropic.ThinkingConfigParamUnion{OfEnabled: &goanthropic.ThinkingConfigEnabledParam{
		BudgetTokens: budget,
		Display:      goanthropic.ThinkingConfigEnabledDisplaySummarized,
	}}
	return true, nil
}

// cacheControl returns the cache breakpoint marker, or nil when retention is none.
func cacheControl(retention chat.CacheRetention) *goanthropic.CacheControlEphemeralParam {
	if retention == chat.CacheRetentionNone {
		return nil
	}
	cc := goanthropic.NewCacheControlEphemeralParam()
	if retention == chat.CacheRetentionLong {
		cc.TTL = goanthropic.CacheControlEphemeralTTLTTL1h
	}
	return &cc
}
