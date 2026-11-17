package anthropic

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"strings"

	goanthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/coldcoder126/rein/chat"
)

// Stream sends a streaming request. Tool call arguments are assembled inside the provider before they are emitted.
func (c *Client) Stream(ctx context.Context, req chat.Request) iter.Seq[chat.Event] {
	return func(yield func(chat.Event) bool) {
		b := chat.NewBuilder(yield, c.info)

		client, err := c.newSDKClient(ctx)
		if err != nil {
			b.Fail(c.wrapError(ctx, err))
			return
		}
		params, err := c.buildParams(req)
		if err != nil {
			b.Fail(&chat.Error{Kind: chat.KindInvalidRequest, Err: err})
			return
		}
		// The response headers carry the request ID, which refusals report.
		var resp *http.Response
		opts := []option.RequestOption{option.WithResponseInto(&resp)}
		if blockBound(params) {
			opts = append(opts, option.WithHeaderAdd("anthropic-beta", prefixMismatchBeta))
		}
		stream := client.Messages.NewStreaming(ctx, params, opts...)
		defer stream.Close()
		var requestID string
		if resp != nil {
			requestID = resp.Header.Get("request-id")
		}

		var usage goanthropic.Usage
		var stopReason goanthropic.StopReason
		var sawStop bool
		var blockType string
		var toolArgs strings.Builder
		var truncated bool
		// serverRaw is the JSON of the open server tool block, completed with its streamed input at content_block_stop.
		var serverRaw string
		var messageID string

		for stream.Next() {
			event := stream.Current()
			switch event.Type {
			case "message_start":
				start := event.AsMessageStart().Message
				usage = start.Usage
				messageID = start.ID
				b.SetResponse(start.ID, string(start.Model))
				b.SetUsage(usageFromAnthropic(usage))
				if !b.Start() {
					return
				}
			case "content_block_start":
				cb := event.AsContentBlockStart().ContentBlock
				blockType = cb.Type
				toolArgs.Reset()
				serverRaw = ""
				if isServerToolBlock(blockType) {
					serverRaw = cb.RawJSON()
				}
				switch blockType {
				case "tool_use":
					tu := cb.AsToolUse()
					if !b.ToolCallStart(tu.ID, tu.Name) {
						return
					}
				case "redacted_thinking":
					if !b.RedactedThinking(cb.AsRedactedThinking().Data) {
						return
					}
				}
			case "content_block_delta":
				delta := event.AsContentBlockDelta().Delta
				ok := true
				switch delta.Type {
				case "text_delta":
					ok = b.TextDelta(delta.AsTextDelta().Text)
				case "thinking_delta":
					ok = b.ThinkingDelta(delta.AsThinkingDelta().Thinking)
				case "signature_delta":
					ok = b.ThinkingSignature(delta.AsSignatureDelta().Signature)
				case "input_json_delta":
					toolArgs.WriteString(delta.AsInputJSONDelta().PartialJSON)
				}
				if !ok {
					return
				}
			case "content_block_stop":
				var ok bool
				if serverRaw != "" {
					raw, name, err := serverToolRecord(serverRaw, toolArgs.String())
					if err != nil {
						b.Fail(&chat.Error{Kind: chat.KindUnknown, Message: "anthropic: " + err.Error()})
						return
					}
					ok = b.ServerTool(name, raw)
				} else if blockType == "tool_use" {
					args := json.RawMessage(toolArgs.String())
					if len(args) > 0 && !json.Valid(args) {
						// When the output is truncated the arguments stop halfway: keep the call with empty arguments and mark the finish
						// as length, so upper layers know the call cannot be executed.
						truncated = true
						args = nil
					}
					ok = b.ToolCallEnd(args)
				} else {
					ok = b.EndBlock()
				}
				if !ok {
					return
				}
				blockType = ""
				serverRaw = ""
			case "message_stop":
				sawStop = true
			case "message_delta":
				md := event.AsMessageDelta()
				stopReason = md.Delta.StopReason
				usage = mergeDeltaUsage(usage, md.Usage)
				b.SetUsage(usageFromAnthropic(usage))
			}
		}
		if err := stream.Err(); err != nil {
			b.Fail(c.wrapError(ctx, fmt.Errorf("anthropic stream: %w", err)))
			return
		}
		if !sawStop {
			b.Fail(&chat.Error{Kind: chat.KindNetwork, Message: "anthropic: stream ended before message_stop"})
			return
		}
		if stopReason == goanthropic.StopReasonRefusal {
			b.Fail(&chat.Error{Kind: chat.KindRefusal, Message: "anthropic: the model refused the request",
				RequestID: cmp.Or(requestID, messageID)})
			return
		}
		reason := stopReasonFromAnthropic(stopReason)
		if truncated {
			reason = chat.StopReasonLength
		}
		b.Done(reason)
	}
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
