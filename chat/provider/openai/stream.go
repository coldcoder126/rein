package openai

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/openai/openai-go/v3/responses"

	"github.com/coldcoder126/rein/chat"
)

// outputItem holds the fields of a response output item this package needs, parsed from raw JSON to avoid the SDK's union accessors.
type outputItem struct {
	Type         string `json:"type"`
	ID           string `json:"id"`
	CallID       string `json:"call_id"`
	Name         string `json:"name"`
	Arguments    string `json:"arguments"`
	Phase        string `json:"phase"`
	Result       string `json:"result"`
	OutputFormat string `json:"output_format"`
	Content      []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

// reasoningText returns the readable text of a reasoning item: the summary first, then plain reasoning content (such as DeepSeek's reasoning_text).
func (it outputItem) reasoningText() string {
	var parts []string
	for _, s := range it.Summary {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	if len(parts) == 0 {
		for _, c := range it.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// streamState translates Responses stream events into chat.Builder calls.
//
// Text and thinking blocks open with their delta events; tool calls and generated images are emitted whole when their output item completes (arguments are not streamed).
// An output item's output_item.done is the authoritative final state; at response.completed the final output list
// fills in items without a done event, for endpoints that omit some events.
type streamState struct {
	b        *chat.Builder
	provider string

	done map[string]bool
	// blockOf maps output items to their text or thinking block positions, used to add signatures and close blocks on completion.
	blockOf  map[string]int
	curItem  string
	toolArgs map[string]*strings.Builder

	refusalStreamed bool
	summaryParts    int
	refusal         strings.Builder

	terminal   bool
	incomplete string
}

func newStreamState(b *chat.Builder, provider string) *streamState {
	return &streamState{
		b:        b,
		provider: provider,
		done:     make(map[string]bool),
		blockOf:  make(map[string]int),
		toolArgs: make(map[string]*strings.Builder),
	}
}

func (s *streamState) itemID(event responses.ResponseStreamEventUnion) string {
	if event.ItemID != "" {
		return event.ItemID
	}
	return s.curItem
}

// handle processes one stream event and returns false when reading should stop.
func (s *streamState) handle(event responses.ResponseStreamEventUnion) bool {
	switch event.Type {
	case "response.created":
		s.b.SetResponse(event.Response.ID, string(event.Response.Model))
		return s.b.Start()

	case "response.output_item.added":
		item := parseItem(event.Item.RawJSON())
		s.curItem = item.ID
		if item.Type == "reasoning" || item.Type == "message" {
			// Every output item gets its own block, even if the previous item had no done event.
			s.summaryParts = 0
			return s.b.EndBlock()
		}

	case "response.reasoning_summary_part.added":
		if s.summaryParts > 0 && !s.b.ThinkingDelta("\n\n") {
			return false
		}
		s.summaryParts++

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if !s.b.ThinkingDelta(event.Delta) {
			return false
		}
		s.blockOf[s.itemID(event)] = s.b.CurrentIndex()

	case "response.output_text.delta":
		if !s.b.TextDelta(event.Delta) {
			return false
		}
		s.blockOf[s.itemID(event)] = s.b.CurrentIndex()

	case "response.refusal.delta":
		s.refusalStreamed = true
		s.refusal.WriteString(event.Delta)

	case "response.function_call_arguments.delta":
		id := s.itemID(event)
		if s.toolArgs[id] == nil {
			s.toolArgs[id] = &strings.Builder{}
		}
		s.toolArgs[id].WriteString(event.Delta)

	case "response.output_item.done":
		return s.itemDone(event.Item.RawJSON())

	case "response.completed", "response.incomplete":
		return s.responseEnded(event)

	case "response.failed":
		s.terminal = true
		return s.fail(string(event.Response.Error.Code), event.Response.Error.Message)

	case "error":
		s.terminal = true
		return s.fail(event.Code, event.Message)
	}
	return true
}

func (s *streamState) responseEnded(event responses.ResponseStreamEventUnion) bool {
	s.terminal = true
	resp := event.Response
	if event.Type == "response.incomplete" {
		s.incomplete = resp.IncompleteDetails.Reason
		if s.incomplete == "" {
			s.incomplete = "unknown"
		}
	}
	for _, out := range resp.Output {
		raw := out.RawJSON()
		if id := parseItem(raw).ID; id == "" || !s.done[id] {
			if !s.itemDone(raw) {
				return false
			}
		}
	}
	usage := resp.Usage
	cached := int(usage.InputTokensDetails.CachedTokens)
	s.b.SetUsage(chat.Usage{
		Input:       int(usage.InputTokens) - cached,
		Output:      int(usage.OutputTokens),
		CacheRead:   cached,
		TotalTokens: int(usage.TotalTokens),
	})
	return true
}

func parseItem(raw string) outputItem {
	var item outputItem
	_ = json.Unmarshal([]byte(raw), &item)
	return item
}

// finishBlock adds the signature to the text or thinking block of an output item and closes the block if it is still open.
func (s *streamState) finishBlock(idx int, signature string) bool {
	s.b.SetSignature(idx, signature)
	if s.b.CurrentIndex() == idx {
		return s.b.EndBlock()
	}
	return true
}

// itemDone closes the content block of an output item using the item's final state.
func (s *streamState) itemDone(raw string) bool {
	item := parseItem(raw)
	if item.ID != "" {
		s.done[item.ID] = true
	}
	switch item.Type {
	case "reasoning":
		idx, streamed := s.blockOf[item.ID]
		if !streamed {
			if !s.b.EndBlock() || !s.b.ThinkingDelta(item.reasoningText()) {
				return false
			}
			if idx = s.b.CurrentIndex(); idx < 0 {
				// Without readable text, an empty thinking block still carries the reasoning item that must be replayed.
				if !s.b.ThinkingSignature(raw) {
					return false
				}
				idx = s.b.CurrentIndex()
			}
		}
		// The full reasoning item (including encrypted content) is everything stateless replay needs.
		s.summaryParts = 0
		return s.finishBlock(idx, raw)

	case "message":
		idx, streamed := s.blockOf[item.ID]
		var text strings.Builder
		for _, c := range item.Content {
			switch c.Type {
			case "output_text":
				text.WriteString(c.Text)
			case "refusal":
				if !s.refusalStreamed {
					s.refusal.WriteString(c.Refusal)
				}
			}
		}
		s.refusalStreamed = false
		if !streamed && text.Len() > 0 {
			if !s.b.EndBlock() || !s.b.TextDelta(text.String()) {
				return false
			}
			idx, streamed = s.b.CurrentIndex(), true
		}
		if !streamed {
			return true
		}
		return s.finishBlock(idx, encodeTextSignature(item.ID, item.Phase))

	case "function_call":
		args := item.Arguments
		if args == "" && s.toolArgs[item.ID] != nil {
			args = s.toolArgs[item.ID].String()
		}
		if !s.b.ToolCallStart(item.CallID, item.Name) {
			return false
		}
		s.b.SignOpenBlock(item.ID)
		return s.b.ToolCallEnd(json.RawMessage(args))

	case "image_generation_call":
		if item.Result == "" {
			return true
		}
		data, err := base64.StdEncoding.DecodeString(item.Result)
		if err != nil {
			return true
		}
		return s.b.Image(imageMIME(item.OutputFormat), data)
	}
	return true
}

func imageMIME(format string) string {
	switch strings.ToLower(format) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func (s *streamState) fail(code, message string) bool {
	text := strings.TrimSpace(code + ": " + message)
	s.b.Fail(&chat.Error{Kind: chat.ClassifyMessage(text), Message: s.provider + ": " + text})
	return false
}

// finish emits the terminal event after the stream was read to the end.
func (s *streamState) finish() {
	switch {
	case !s.terminal:
		s.b.Fail(&chat.Error{Kind: chat.KindNetwork, Message: "openai: stream ended before response.completed"})
	case s.refusal.Len() > 0:
		s.b.Fail(&chat.Error{Kind: chat.KindRefusal, Message: "openai: the model refused the request: " + s.refusal.String()})
	case s.incomplete == "content_filter":
		s.b.Fail(&chat.Error{Kind: chat.KindRefusal, Message: "openai: response blocked by content filtering"})
	case s.incomplete == "max_output_tokens":
		s.b.Done(chat.StopReasonLength)
	case s.incomplete != "":
		s.b.Fail(&chat.Error{Kind: chat.KindUnknown, Message: "openai: response incomplete: " + s.incomplete})
	default:
		s.b.Done(chat.StopReasonStop)
	}
}
