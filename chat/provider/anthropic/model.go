package anthropic

import (
	"strings"

	goanthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/coldcoder126/rein/chat"
)

// modelSpec is a model's built-in spec, taken from Anthropic's model list API (/v1/models).
type modelSpec struct {
	maxInput  int
	maxOutput int
	// budgetThinking means the model only supports budget thinking (thinking.type=enabled + budget_tokens);
	// otherwise adaptive thinking is used (thinking.type=adaptive + output_config.effort).
	budgetThinking bool
	// noXHigh means effort does not support xhigh.
	noXHigh bool
	// alwaysThinks means the model does not accept thinking.type=disabled.
	alwaysThinks bool
	// structuredOutput means the model supports JSON Schema constraints via output_config.format.
	structuredOutput bool
	// toolSearch means the model supports the server-side tool search, which deferred tools need.
	toolSearch bool
	// thinksByDefault means the model thinks when the request sets no thinking (Claude 5 and later). These models accept
	// thinking.block_binding, and some bind thinking blocks to the conversation that produced them.
	thinksByDefault bool
}

// specFor returns the spec for a model name. Unknown Claude models are treated as the latest generation; non-Claude models (compatible endpoints) get conservative values.
func specFor(model string) modelSpec {
	current := modelSpec{maxInput: 1_000_000, maxOutput: 128_000, structuredOutput: true, toolSearch: true, thinksByDefault: !strings.Contains(model, "-4-")}
	switch {
	case !strings.HasPrefix(model, "claude-"):
		return modelSpec{maxInput: 200_000, maxOutput: 8_192, budgetThinking: true}
	case strings.HasPrefix(model, "claude-3"):
		return modelSpec{maxInput: 200_000, maxOutput: 64_000, budgetThinking: true, structuredOutput: true}
	case strings.Contains(model, "-4-5"):
		return modelSpec{maxInput: 200_000, maxOutput: 64_000, budgetThinking: true, structuredOutput: true, toolSearch: true}
	case strings.Contains(model, "-4-6"):
		current.noXHigh = true
	case strings.Contains(model, "sonnet-5-5"), strings.Contains(model, "opus-5-5"), strings.Contains(model, "fable"):
		current.alwaysThinks = true
	}
	return current
}

// spec returns the spec of the model used by this request; values set explicitly in Config take precedence.
func (c *Client) spec(model string) modelSpec {
	s := specFor(model)
	if c.cfg.MaxInputTokens > 0 {
		s.maxInput = c.cfg.MaxInputTokens
	}
	if c.cfg.MaxOutputTokens > 0 {
		s.maxOutput = c.cfg.MaxOutputTokens
	}
	if c.cfg.BudgetThinking {
		s.budgetThinking = true
	}
	return s
}

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
}

// applyReasoning sets thinking / output_config from the provider-neutral effort and reports whether thinking is on for this request.
// maxTokensExplicit means max_tokens is the answer limit set by the caller: budget thinking shares max_tokens with the answer,
// so the budget is added to the limit to keep thinking from crowding out the answer.
func applyReasoning(params *goanthropic.MessageNewParams, level chat.ReasoningLevel, spec modelSpec, maxTokensExplicit bool) (bool, error) {
	switch level {
	case "":
		return false, nil
	case chat.ReasoningOff:
		if spec.alwaysThinks {
			params.OutputConfig.Effort = goanthropic.OutputConfigEffortLow
			return true, nil
		}
		params.Thinking = goanthropic.ThinkingConfigParamUnion{OfDisabled: &goanthropic.ThinkingConfigDisabledParam{}}
		return false, nil
	}
	budget, ok := thinkingBudgets[level]
	if !ok {
		return false, &chat.Error{Kind: chat.KindInvalidRequest, Message: "anthropic: unknown reasoning level " + string(level)}
	}

	if !spec.budgetThinking {
		effort := goanthropic.OutputConfigEffort(level)
		if level == chat.ReasoningXHigh && spec.noXHigh {
			effort = goanthropic.OutputConfigEffortHigh
		}
		params.Thinking = goanthropic.ThinkingConfigParamUnion{OfAdaptive: &goanthropic.ThinkingConfigAdaptiveParam{
			Display: goanthropic.ThinkingConfigAdaptiveDisplaySummarized,
		}}
		params.OutputConfig.Effort = effort
		return true, nil
	}

	answer := int64(minAnswerTokens)
	if maxTokensExplicit {
		answer = min(answer, params.MaxTokens)
		params.MaxTokens = min(params.MaxTokens+budget, int64(spec.maxOutput))
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
