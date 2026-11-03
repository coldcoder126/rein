package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	goanthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/coldcoder126/rein/chat"
)

func (c *Client) buildParams(req chat.Request) (goanthropic.MessageNewParams, error) {
	target := c.modelInfo(req)
	spec := c.spec(target.Model)
	if !spec.structuredOutput {
		req = chat.WithOutputInstructions(req)
	}

	cc := cacheControl(req.CacheRetention)
	var systemBlocks []goanthropic.TextBlockParam
	if strings.TrimSpace(req.SystemPrompt) != "" {
		block := goanthropic.TextBlockParam{Text: req.SystemPrompt}
		if cc != nil {
			block.CacheControl = *cc
		}
		systemBlocks = append(systemBlocks, block)
	}

	messages := chat.NormalizeForModel(req.Messages, target, chat.WithToolCallIDNormalizer(normalizeToolCallID))
	msgs, err := convertMessages(messages, c.cfg.UnsignedThinking)
	if err != nil {
		return goanthropic.MessageNewParams{}, err
	}
	markLastBlock(msgs, cc)

	maxTokens := int64(spec.maxOutput)
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}

	params := goanthropic.MessageNewParams{
		Model:     c.resolveModel(req),
		MaxTokens: maxTokens,
		Messages:  msgs,
		System:    systemBlocks,
	}
	if req.Output != nil {
		var schema map[string]any
		if err := json.Unmarshal(req.Output.Schema, &schema); err != nil {
			return goanthropic.MessageNewParams{}, fmt.Errorf("anthropic: invalid output schema: %w", err)
		}
		params.OutputConfig.Format = goanthropic.JSONOutputFormatParam{Schema: schema}
	}
	reasoning := req.Reasoning
	if forced := len(req.Tools) > 0 && (req.ToolChoice.Mode == chat.ToolChoiceRequired || req.ToolChoice.Mode == chat.ToolChoiceTool); forced {
		// Whether forced tool calls work with thinking depends on the model (tested): models that cannot turn thinking off do not support it;
		// budget thinking models cannot enable both, so thinking is off for this request; adaptive thinking models allow both.
		if spec.alwaysThinks {
			return goanthropic.MessageNewParams{}, fmt.Errorf("anthropic: model %s does not support forced tool calls (ToolChoice %s)", target.Model, req.ToolChoice.Mode)
		}
		if spec.budgetThinking && reasoning != chat.ReasoningOff {
			reasoning = ""
		}
	}
	thinking, err := applyReasoning(&params, reasoning, spec, req.MaxTokens > 0)
	if err != nil {
		return goanthropic.MessageNewParams{}, err
	}
	// The API does not accept a custom temperature with thinking on.
	if req.Temperature != nil && !thinking {
		params.Temperature = goanthropic.Float(*req.Temperature)
	}
	if len(req.Tools) > 0 {
		tools, err := convertTools(req.Tools)
		if err != nil {
			return goanthropic.MessageNewParams{}, err
		}
		params.Tools = tools
		switch req.ToolChoice.Mode {
		case "":
		case chat.ToolChoiceAuto:
			params.ToolChoice = goanthropic.ToolChoiceUnionParam{OfAuto: &goanthropic.ToolChoiceAutoParam{}}
		case chat.ToolChoiceNone:
			params.ToolChoice = goanthropic.ToolChoiceUnionParam{OfNone: &goanthropic.ToolChoiceNoneParam{}}
		case chat.ToolChoiceRequired:
			params.ToolChoice = goanthropic.ToolChoiceUnionParam{OfAny: &goanthropic.ToolChoiceAnyParam{}}
		case chat.ToolChoiceTool:
			params.ToolChoice = goanthropic.ToolChoiceUnionParam{OfTool: &goanthropic.ToolChoiceToolParam{Name: req.ToolChoice.Name}}
		default:
			return goanthropic.MessageNewParams{}, fmt.Errorf("anthropic: unknown ToolChoice %q", req.ToolChoice.Mode)
		}
	}
	return params, nil
}

// markLastBlock puts a cache breakpoint on the last block of the last user message, making the whole history a cacheable prefix.
// Tool definitions and the system prompt come earlier in the prefix and are covered by the breakpoint on the system prompt.
func markLastBlock(msgs []goanthropic.MessageParam, cc *goanthropic.CacheControlEphemeralParam) {
	if cc == nil || len(msgs) == 0 {
		return
	}
	last := msgs[len(msgs)-1]
	if last.Role != goanthropic.MessageParamRoleUser || len(last.Content) == 0 {
		return
	}
	if slot := last.Content[len(last.Content)-1].GetCacheControl(); slot != nil {
		*slot = *cc
	}
}

func convertMessages(messages []chat.Message, unsignedThinking bool) ([]goanthropic.MessageParam, error) {
	var msgs []goanthropic.MessageParam
	for i := 0; i < len(messages); {
		m := messages[i]
		if m.Role == chat.RoleTool {
			var blocks []goanthropic.ContentBlockParamUnion
			for i < len(messages) && messages[i].Role == chat.RoleTool {
				block, err := toolResultBlock(messages[i])
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, block)
				i++
			}
			msgs = append(msgs, goanthropic.NewUserMessage(blocks...))
			continue
		}
		param, ok, err := convertMessage(m, unsignedThinking)
		if err != nil {
			return nil, err
		}
		if ok {
			msgs = append(msgs, param)
		}
		i++
	}
	return msgs, nil
}

func convertMessage(m chat.Message, unsignedThinking bool) (goanthropic.MessageParam, bool, error) {
	switch m.Role {
	case chat.RoleUser:
		var blocks []goanthropic.ContentBlockParamUnion
		for _, block := range m.Content {
			switch block.Type {
			case chat.ContentTypeText:
				if block.Text != "" {
					blocks = append(blocks, goanthropic.NewTextBlock(block.Text))
				}
			case chat.ContentTypeMedia:
				image, document, err := mediaBlock(block)
				if err != nil {
					return goanthropic.MessageParam{}, false, err
				}
				blocks = append(blocks, goanthropic.ContentBlockParamUnion{OfImage: image, OfDocument: document})
			default:
				return goanthropic.MessageParam{}, false, fmt.Errorf("anthropic: user messages do not support content block type %q", block.Type)
			}
		}
		if len(blocks) == 0 {
			return goanthropic.MessageParam{}, false, nil
		}
		return goanthropic.NewUserMessage(blocks...), true, nil
	case chat.RoleAssistant:
		blocks := make([]goanthropic.ContentBlockParamUnion, 0, len(m.Content))
		for _, block := range m.Content {
			switch block.Type {
			case chat.ContentTypeThinking:
				switch {
				case block.Redacted:
					blocks = append(blocks, goanthropic.NewRedactedThinkingBlock(block.Signature))
				case block.Signature != "" || unsignedThinking:
					blocks = append(blocks, goanthropic.NewThinkingBlock(block.Signature, block.Text))
				case block.Text != "":
					// The API rejects thinking blocks without a signature, so they are downgraded to plain text.
					blocks = append(blocks, goanthropic.NewTextBlock(block.Text))
				}
			case chat.ContentTypeText:
				if block.Text != "" {
					blocks = append(blocks, goanthropic.NewTextBlock(block.Text))
				}
			case chat.ContentTypeToolCall:
				if block.ToolCall == nil {
					continue
				}
				var input any
				if err := json.Unmarshal(block.ToolCall.Arguments, &input); err != nil {
					input = map[string]any{}
				}
				blocks = append(blocks, goanthropic.NewToolUseBlock(block.ToolCall.ID, input, block.ToolCall.Name))
			default:
				return goanthropic.MessageParam{}, false, fmt.Errorf("anthropic: assistant messages do not support content block type %q", block.Type)
			}
		}
		if len(blocks) == 0 {
			return goanthropic.MessageParam{}, false, nil
		}
		return goanthropic.NewAssistantMessage(blocks...), true, nil
	default:
		return goanthropic.MessageParam{}, false, fmt.Errorf("anthropic: unknown message role %q", m.Role)
	}
}

// toolResultBlock converts a tool result into a tool_result block, mixing text, images and documents.
// A placeholder text is added when there is no text, since the API rejects tool results without any.
func toolResultBlock(m chat.Message) (goanthropic.ContentBlockParamUnion, error) {
	var content []goanthropic.ToolResultBlockParamContentUnion
	hasText, hasMedia := false, false
	addText := func(text string) {
		hasText = true
		content = append(content, goanthropic.ToolResultBlockParamContentUnion{OfText: &goanthropic.TextBlockParam{Text: text}})
	}
	for _, block := range m.Content {
		switch block.Type {
		case chat.ContentTypeText:
			if block.Text != "" {
				addText(block.Text)
			}
		case chat.ContentTypeMedia:
			image, document, err := mediaBlock(block)
			if err != nil {
				return goanthropic.ContentBlockParamUnion{}, err
			}
			hasMedia = true
			content = append(content, goanthropic.ToolResultBlockParamContentUnion{OfImage: image, OfDocument: document})
		default:
			return goanthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: tool results do not support content block type %q", block.Type)
		}
	}
	switch {
	case hasMedia && !hasText:
		content = append([]goanthropic.ToolResultBlockParamContentUnion{
			{OfText: &goanthropic.TextBlockParam{Text: "(see attached content)"}},
		}, content...)
	case len(content) == 0:
		content = []goanthropic.ToolResultBlockParamContentUnion{{OfText: &goanthropic.TextBlockParam{Text: ""}}}
	}
	return goanthropic.ContentBlockParamUnion{OfToolResult: &goanthropic.ToolResultBlockParam{
		ToolUseID: m.ToolCallID,
		Content:   content,
		IsError:   goanthropic.Bool(m.IsError),
	}}, nil
}

// mediaBlock converts a media block into an Anthropic image or document block, exactly one of which is non-nil: images and PDFs
// support inline data and URLs, text files become plain text documents (inline only). Media the Messages API has no input for returns an error and the caller decides what to do.
func mediaBlock(block chat.ContentBlock) (*goanthropic.ImageBlockParam, *goanthropic.DocumentBlockParam, error) {
	inline := len(block.Data) > 0
	if !inline && block.URL == "" {
		return nil, nil, &chat.Error{Kind: chat.KindInvalidRequest, Message: "anthropic: media block has neither data nor URL"}
	}
	if block.IsImage() {
		image := &goanthropic.ImageBlockParam{}
		if inline {
			image.Source.OfBase64 = &goanthropic.Base64ImageSourceParam{
				Data:      base64.StdEncoding.EncodeToString(block.Data),
				MediaType: goanthropic.Base64ImageSourceMediaType(block.MIMEType),
			}
		} else {
			image.Source.OfURL = &goanthropic.URLImageSourceParam{URL: block.URL}
		}
		return image, nil, nil
	}
	document := &goanthropic.DocumentBlockParam{}
	if block.Name != "" {
		document.Title = goanthropic.String(block.Name)
	}
	switch {
	case block.MIMEType == "application/pdf" && inline:
		document.Source.OfBase64 = &goanthropic.Base64PDFSourceParam{Data: base64.StdEncoding.EncodeToString(block.Data)}
	case block.MIMEType == "application/pdf":
		document.Source.OfURL = &goanthropic.URLPDFSourceParam{URL: block.URL}
	case strings.HasPrefix(block.MIMEType, "text/") && inline:
		document.Source.OfText = &goanthropic.PlainTextSourceParam{Data: string(block.Data)}
	default:
		source := "inline"
		if !inline {
			source = "URL"
		}
		return nil, nil, &chat.Error{Kind: chat.KindInvalidRequest, Message: fmt.Sprintf(
			"anthropic: the Messages API does not support %s %s media (it supports images, PDFs, and inline text files)", source, block.MIMEType)}
	}
	return nil, document, nil
}

// convertTools passes the JSON Schema through in full: fields other than properties/required ($defs, additionalProperties and so on)
// are kept via ExtraFields.
func convertTools(defs []chat.ToolDef) ([]goanthropic.ToolUnionParam, error) {
	out := make([]goanthropic.ToolUnionParam, len(defs))
	for i, def := range defs {
		var schema map[string]any
		if len(def.InputSchema) > 0 {
			if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
				return nil, fmt.Errorf("tool %q: invalid input_schema: %w", def.Name, err)
			}
		}
		inputSchema := goanthropic.ToolInputSchemaParam{}
		if schema != nil {
			inputSchema.Properties = schema["properties"]
			if req, ok := schema["required"].([]any); ok {
				for _, r := range req {
					if name, ok := r.(string); ok {
						inputSchema.Required = append(inputSchema.Required, name)
					}
				}
			}
			for k, v := range schema {
				if k == "type" || k == "properties" || k == "required" {
					continue
				}
				if inputSchema.ExtraFields == nil {
					inputSchema.ExtraFields = make(map[string]any)
				}
				inputSchema.ExtraFields[k] = v
			}
		}
		tp := goanthropic.ToolParam{Name: def.Name, InputSchema: inputSchema}
		if def.Description != "" {
			tp.Description = goanthropic.String(def.Description)
		}
		if len(def.InputExamples) > 0 {
			tp.InputExamples = def.InputExamples
		}
		out[i] = goanthropic.ToolUnionParam{OfTool: &tp}
	}
	return out, nil
}

func usageFromAnthropic(usage goanthropic.Usage) chat.Usage {
	return chat.Usage{
		Input:       int(usage.InputTokens),
		Output:      int(usage.OutputTokens),
		CacheRead:   int(usage.CacheReadInputTokens),
		CacheWrite:  int(usage.CacheCreationInputTokens),
		TotalTokens: int(usage.InputTokens + usage.OutputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens),
	}
}

// mergeDeltaUsage updates the usage from message_start with the cumulative values in message_delta, keeping fields the server did not send.
func mergeDeltaUsage(base goanthropic.Usage, delta goanthropic.MessageDeltaUsage) goanthropic.Usage {
	if delta.InputTokens > 0 {
		base.InputTokens = delta.InputTokens
	}
	if delta.CacheReadInputTokens > 0 {
		base.CacheReadInputTokens = delta.CacheReadInputTokens
	}
	if delta.CacheCreationInputTokens > 0 {
		base.CacheCreationInputTokens = delta.CacheCreationInputTokens
	}
	base.OutputTokens = delta.OutputTokens
	return base
}

func stopReasonFromAnthropic(reason goanthropic.StopReason) chat.StopReason {
	switch reason {
	case goanthropic.StopReasonToolUse:
		return chat.StopReasonToolUse
	case goanthropic.StopReasonMaxTokens, goanthropic.StopReasonModelContextWindowExceeded:
		// Output that hits the context window is a truncated but valid reply, just like hitting max_tokens.
		return chat.StopReasonLength
	case goanthropic.StopReasonRefusal:
		return chat.StopReasonError
	default:
		return chat.StopReasonStop
	}
}

// normalizeToolCallID normalizes tool call IDs produced by other providers into the format Anthropic accepts:
// letters, digits, underscores and hyphens only, at most 64 characters.
func normalizeToolCallID(id string) string {
	var sb strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	out := sb.String()
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
