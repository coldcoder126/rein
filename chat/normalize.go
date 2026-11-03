package chat

import (
	"slices"
	"strings"
)

const (
	generatedImagePlaceholder = "(generated image omitted: model does not support image replay)"
	noToolResultText          = "No result provided"
)

// NormalizeOption adjusts the behavior of NormalizeForModel.
type NormalizeOption func(*normalizeConfig)

type normalizeConfig struct {
	toolCallID func(string) string
}

// WithToolCallIDNormalizer sets a function that normalizes tool call IDs.
// It only applies to history from other models; Anthropic, for example, requires IDs made of letters, digits, underscores and hyphens.
func WithToolCallIDNormalizer(fn func(id string) string) NormalizeOption {
	return func(c *normalizeConfig) { c.toolCallID = fn }
}

// NormalizeForModel turns history into a form the target model accepts, without modifying its input:
//
//   - drops error / aborted assistant messages: they are incomplete turns and replaying them makes the API fail;
//   - adds an error result for tool calls without one, and drops orphan tool results without a matching call;
//   - downgrades thinking blocks from other models to text and drops redacted ones; tool call IDs can be normalized,
//     and provider-private signatures on text and tool call blocks, valid only for the original model, are cleared across models;
//   - when the target endpoint does not support media input (see ModelInfo.SupportsMedia), replaces media in user messages and tool results with a note;
//   - when the target model cannot replay generated images, replaces images in assistant messages with placeholder text; otherwise keeps them, without signatures across models.
func NormalizeForModel(msgs []Message, target ModelInfo, opts ...NormalizeOption) []Message {
	var cfg normalizeConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	idMap := make(map[string]string)
	transformed := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		switch msg.Role {
		case RoleUser:
			if !target.SupportsMedia {
				msg.Content = replaceMedia(msg.Content)
			}
		case RoleTool:
			if !target.SupportsMedia {
				msg.Content = replaceMedia(msg.Content)
			}
			if id, ok := idMap[msg.ToolCallID]; ok {
				msg.ToolCallID = id
			}
		case RoleAssistant:
			if msg.StopReason == StopReasonError || msg.StopReason == StopReasonAborted {
				continue
			}
			sameModel := msg.Provider == target.Provider && msg.Model == target.Model
			msg.Content = transformAssistantContent(msg.Content, sameModel, target.ImageOutput, cfg, idMap)
			if len(msg.Content) == 0 {
				continue
			}
		}
		transformed = append(transformed, msg)
	}
	return closeToolCalls(transformed)
}

func transformAssistantContent(blocks []ContentBlock, sameModel, keepImages bool, cfg normalizeConfig, idMap map[string]string) []ContentBlock {
	out := make([]ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ContentTypeThinking:
			switch {
			case block.Redacted:
				if sameModel {
					out = append(out, block)
				}
			case sameModel && block.Signature != "":
				out = append(out, block)
			case strings.TrimSpace(block.Text) == "":
			case sameModel:
				out = append(out, block)
			default:
				out = append(out, Text(block.Text))
			}
		case ContentTypeText:
			if !sameModel {
				block.Signature = ""
			}
			out = append(out, block)
		case ContentTypeToolCall:
			if block.ToolCall == nil {
				continue
			}
			if !sameModel {
				block.Signature = ""
			}
			if !sameModel && cfg.toolCallID != nil {
				call := *block.ToolCall
				if id := cfg.toolCallID(call.ID); id != call.ID {
					idMap[call.ID] = id
					call.ID = id
				}
				block.ToolCall = &call
			}
			out = append(out, block)
		case ContentTypeMedia:
			switch {
			case !keepImages:
				if n := len(out); n == 0 || out[n-1].Type != ContentTypeText || out[n-1].Text != generatedImagePlaceholder {
					out = append(out, Text(generatedImagePlaceholder))
				}
			case sameModel:
				out = append(out, block)
			default:
				block.Signature = ""
				out = append(out, block)
			}
		default:
			out = append(out, block)
		}
	}
	return out
}

// closeToolCalls makes every tool call be followed by its result, adding error results for missing ones and dropping orphan results.
func closeToolCalls(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	var pending []ToolCall
	answered := make(map[string]bool)

	flush := func() {
		for _, call := range pending {
			if !answered[call.ID] {
				out = append(out, Message{
					Role:       RoleTool,
					Content:    []ContentBlock{Text(noToolResultText)},
					ToolCallID: call.ID,
					ToolName:   call.Name,
					IsError:    true,
				})
			}
		}
		pending = nil
		answered = make(map[string]bool)
	}

	for _, msg := range msgs {
		switch msg.Role {
		case RoleAssistant:
			flush()
			pending = msg.ToolCalls()
			out = append(out, msg)
		case RoleTool:
			if !isPending(pending, msg.ToolCallID) {
				continue
			}
			answered[msg.ToolCallID] = true
			out = append(out, msg)
		default:
			flush()
			out = append(out, msg)
		}
	}
	flush()
	return out
}

func isPending(pending []ToolCall, id string) bool {
	for _, call := range pending {
		if call.ID == id {
			return true
		}
	}
	return false
}

// replaceMedia replaces media blocks with a note, returning the blocks unchanged when there are none.
func replaceMedia(blocks []ContentBlock) []ContentBlock {
	if !slices.ContainsFunc(blocks, ContentBlock.IsMedia) {
		return blocks
	}
	out := make([]ContentBlock, len(blocks))
	for i, block := range blocks {
		if block.IsMedia() {
			block = mediaPlaceholder(block)
		}
		out[i] = block
	}
	return out
}
