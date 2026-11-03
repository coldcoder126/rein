package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"github.com/coldcoder126/rein/chat"
)

const (
	// generatedIDPrefix marks IDs this package generates for function calls without an id; they are not sent to the server on replay.
	generatedIDPrefix = "gemini_call_"
	// skipSignature is the official placeholder signature: Gemini 3 requires a thought signature on function calls,
	// and historical calls from other models without one use it to skip validation.
	skipSignature = "skip_thought_signature_validator"
)

var thinkingBudgets = map[chat.ReasoningLevel]int32{
	chat.ReasoningLow:    2_048,
	chat.ReasoningMedium: 8_192,
	chat.ReasoningHigh:   16_384,
	chat.ReasoningXHigh:  24_576,
}

var thinkingLevels = map[chat.ReasoningLevel]genai.ThinkingLevel{
	chat.ReasoningLow:    genai.ThinkingLevelLow,
	chat.ReasoningMedium: genai.ThinkingLevelMedium,
	chat.ReasoningHigh:   genai.ThinkingLevelHigh,
	chat.ReasoningXHigh:  genai.ThinkingLevelHigh,
}

// usesThinkingBudget means the model only accepts a thinking budget (Gemini 2.x); 3.x and later use thinkingLevel.
func usesThinkingBudget(model string) bool {
	return strings.HasPrefix(model, "gemini-2")
}

// thinkingConfig maps the provider-neutral effort to Gemini's thinking config, always asking for thought summaries.
func thinkingConfig(model string, level chat.ReasoningLevel) (*genai.ThinkingConfig, error) {
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
	if usesThinkingBudget(model) {
		return &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: genai.Ptr(thinkingBudgets[level])}, nil
	}
	return &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: thinkingLevels[level]}, nil
}

func (c *Client) buildRequest(req chat.Request) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	model := c.resolveModel(req)
	config := &genai.GenerateContentConfig{}
	if strings.TrimSpace(req.SystemPrompt) != "" {
		config.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: req.SystemPrompt}}}
	}
	if req.MaxTokens > 0 {
		config.MaxOutputTokens = int32(req.MaxTokens)
	}
	if req.Temperature != nil {
		config.Temperature = genai.Ptr(float32(*req.Temperature))
	}
	if req.Output != nil {
		var schema any
		if err := json.Unmarshal(req.Output.Schema, &schema); err != nil {
			return nil, nil, fmt.Errorf("gemini: invalid output schema: %w", err)
		}
		config.ResponseMIMEType = "application/json"
		config.ResponseJsonSchema = schema
	}
	// Gemini 2.x image models do not support thinking and reject a thinking config.
	if !(c.cfg.ImageGeneration && usesThinkingBudget(model) && req.Reasoning == "") {
		thinking, err := thinkingConfig(model, req.Reasoning)
		if err != nil {
			return nil, nil, err
		}
		config.ThinkingConfig = thinking
	}
	if c.cfg.ImageGeneration {
		config.ResponseModalities = []string{"TEXT", "IMAGE"}
	}
	if len(req.Tools) > 0 {
		tool, err := convertTools(req.Tools)
		if err != nil {
			return nil, nil, err
		}
		config.Tools = []*genai.Tool{tool}
		fc := &genai.FunctionCallingConfig{}
		switch req.ToolChoice.Mode {
		case "":
			fc = nil
		case chat.ToolChoiceAuto:
			fc.Mode = genai.FunctionCallingConfigModeAuto
		case chat.ToolChoiceNone:
			fc.Mode = genai.FunctionCallingConfigModeNone
		case chat.ToolChoiceRequired:
			fc.Mode = genai.FunctionCallingConfigModeAny
		case chat.ToolChoiceTool:
			fc.Mode = genai.FunctionCallingConfigModeAny
			fc.AllowedFunctionNames = []string{req.ToolChoice.Name}
		default:
			return nil, nil, fmt.Errorf("gemini: unknown ToolChoice %q", req.ToolChoice.Mode)
		}
		if fc != nil {
			config.ToolConfig = &genai.ToolConfig{FunctionCallingConfig: fc}
		}
	}

	target := c.Info()
	target.Model = model
	contents, err := convertMessages(chat.NormalizeForModel(req.Messages, target), !usesThinkingBudget(model))
	if err != nil {
		return nil, nil, err
	}
	return contents, config, nil
}

func convertTools(defs []chat.ToolDef) (*genai.Tool, error) {
	tool := &genai.Tool{}
	for _, def := range defs {
		decl := &genai.FunctionDeclaration{Name: def.Name, Description: def.Description}
		if len(def.InputSchema) > 0 {
			var schema map[string]any
			if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
				return nil, fmt.Errorf("tool %q: invalid input_schema: %w", def.Name, err)
			}
			decl.ParametersJsonSchema = schema
		}
		tool.FunctionDeclarations = append(tool.FunctionDeclarations, decl)
	}
	return tool, nil
}

// convertMessages converts normalized history into Gemini contents.
// Consecutive tool results are merged into one user content, as Gemini requires for parallel function calls.
// multimodalResults means the model accepts images embedded in function results (Gemini 3 and later); otherwise images are separate parts of the same message.
func convertMessages(messages []chat.Message, multimodalResults bool) ([]*genai.Content, error) {
	var contents []*genai.Content
	for i := 0; i < len(messages); {
		m := messages[i]
		switch m.Role {
		case chat.RoleUser:
			if parts := mediaParts(m.Content); len(parts) > 0 {
				contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: parts})
			}
			i++
		case chat.RoleAssistant:
			if parts := assistantParts(m.Content); len(parts) > 0 {
				contents = append(contents, &genai.Content{Role: genai.RoleModel, Parts: parts})
			}
			i++
		case chat.RoleTool:
			var parts, images []*genai.Part
			for ; i < len(messages) && messages[i].Role == chat.RoleTool; i++ {
				part, extra := toolResultPart(messages[i], multimodalResults)
				parts = append(parts, part)
				images = append(images, extra...)
			}
			contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: append(parts, images...)})
		default:
			return nil, fmt.Errorf("gemini: unknown message role %q", m.Role)
		}
	}
	return contents, nil
}

// mediaPart converts a media block into a part: inline data or a file URI, the same path for every MIME type.
func mediaPart(block chat.ContentBlock) *genai.Part {
	if block.URL != "" {
		return &genai.Part{FileData: &genai.FileData{MIMEType: block.MIMEType, FileURI: block.URL}}
	}
	return &genai.Part{InlineData: &genai.Blob{MIMEType: block.MIMEType, Data: block.Data}}
}

// mediaParts converts text and media blocks, ignoring other types.
func mediaParts(blocks []chat.ContentBlock) []*genai.Part {
	var parts []*genai.Part
	for _, block := range blocks {
		switch block.Type {
		case chat.ContentTypeText:
			if block.Text != "" {
				parts = append(parts, &genai.Part{Text: block.Text})
			}
		case chat.ContentTypeMedia:
			parts = append(parts, mediaPart(block))
		}
	}
	return parts
}

// assistantParts replays an assistant message. A block's signature is a base64 thoughtSignature and must stay on its original part.
func assistantParts(blocks []chat.ContentBlock) []*genai.Part {
	signed := false
	for _, block := range blocks {
		signed = signed || block.Signature != ""
	}
	var parts []*genai.Part
	for _, block := range blocks {
		sig := decodeSignature(block.Signature)
		switch block.Type {
		case chat.ContentTypeThinking:
			// Thought summaries need not be sent back; only signed ones are worth replaying.
			if sig != nil {
				parts = append(parts, &genai.Part{Text: block.Text, Thought: true, ThoughtSignature: sig})
			}
		case chat.ContentTypeText:
			if block.Text != "" || sig != nil {
				parts = append(parts, &genai.Part{Text: block.Text, ThoughtSignature: sig})
			}
		case chat.ContentTypeMedia:
			part := mediaPart(block)
			part.ThoughtSignature = sig
			parts = append(parts, part)
		case chat.ContentTypeToolCall:
			if block.ToolCall == nil {
				continue
			}
			var args map[string]any
			_ = json.Unmarshal(block.ToolCall.Arguments, &args)
			if sig == nil && !signed {
				// No signature anywhere in the message (it came from another model): give the first function call the placeholder signature.
				sig, signed = []byte(skipSignature), true
			}
			parts = append(parts, &genai.Part{
				FunctionCall:     &genai.FunctionCall{ID: wireID(block.ToolCall.ID), Name: block.ToolCall.Name, Args: args},
				ThoughtSignature: sig,
			})
		}
	}
	return parts
}

// toolResultPart converts a tool result into a functionResponse, with media embedded or returned as extra parts depending on the model.
func toolResultPart(m chat.Message, multimodalResults bool) (*genai.Part, []*genai.Part) {
	var text []string
	var images []*genai.Part
	resp := &genai.FunctionResponse{ID: wireID(m.ToolCallID), Name: m.ToolName}
	for _, block := range m.Content {
		switch block.Type {
		case chat.ContentTypeText:
			if block.Text != "" {
				text = append(text, block.Text)
			}
		case chat.ContentTypeMedia:
			// Only inline data can be embedded in a functionResponse; URL media and media for older models follow as extra parts.
			if multimodalResults && len(block.Data) > 0 {
				resp.Parts = append(resp.Parts, &genai.FunctionResponsePart{
					InlineData: &genai.FunctionResponseBlob{MIMEType: block.MIMEType, Data: block.Data},
				})
			} else {
				images = append(images, mediaPart(block))
			}
		}
	}
	output := strings.Join(text, "\n")
	if output == "" && len(resp.Parts)+len(images) > 0 {
		output = "(see attached content)"
	}
	key := "output"
	if m.IsError {
		key = "error"
	}
	resp.Response = map[string]any{key: output}
	return &genai.Part{FunctionResponse: resp}, images
}

// wireID returns the function call ID sent to the server: IDs generated by this package are unknown to the server and not sent.
func wireID(id string) string {
	if strings.HasPrefix(id, generatedIDPrefix) {
		return ""
	}
	return id
}

func encodeSignature(sig []byte) string {
	if len(sig) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func decodeSignature(sig string) []byte {
	if sig == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return nil
	}
	return data
}
