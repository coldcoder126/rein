package gemini

import (
	"encoding/json"
	"fmt"

	"google.golang.org/genai"

	"github.com/coldcoder126/rein/chat"
)

// streamState translates Gemini's streaming response into chat.Builder calls.
//
// Each chunk carries parts: thought summaries, text, function calls (with complete arguments) or generated images.
// A thoughtSignature may appear on any part, or alone on an empty part, and must be sent back on replay,
// so it is recorded as the signature of the corresponding content block.
type streamState struct {
	b *chat.Builder
	// last is the position of the latest content block, which owns empty parts that carry only a signature.
	last int
	// calls is used to generate IDs for function calls without an id.
	calls int

	finishReason  genai.FinishReason
	finishMessage string
	blockReason   genai.BlockedReason
}

// handle processes one response chunk and returns false when reading should stop.
func (s *streamState) handle(resp *genai.GenerateContentResponse) bool {
	if resp.ResponseID != "" {
		s.b.SetResponse(resp.ResponseID, resp.ModelVersion)
	}
	if !s.b.Start() {
		return false
	}
	if u := resp.UsageMetadata; u != nil {
		cached := int(u.CachedContentTokenCount)
		s.b.SetUsage(chat.Usage{
			Input:       int(u.PromptTokenCount+u.ToolUsePromptTokenCount) - cached,
			Output:      int(u.CandidatesTokenCount + u.ThoughtsTokenCount),
			CacheRead:   cached,
			TotalTokens: int(u.TotalTokenCount),
		})
	}
	if fb := resp.PromptFeedback; fb != nil && fb.BlockReason != "" {
		s.blockReason = fb.BlockReason
	}
	if len(resp.Candidates) == 0 {
		return true
	}
	cand := resp.Candidates[0]
	if cand.FinishReason != "" {
		s.finishReason, s.finishMessage = cand.FinishReason, cand.FinishMessage
	}
	if cand.Content == nil {
		return true
	}
	for _, part := range cand.Content.Parts {
		if part != nil && !s.part(part) {
			return false
		}
	}
	return true
}

func (s *streamState) part(part *genai.Part) bool {
	sig := encodeSignature(part.ThoughtSignature)
	switch {
	case part.FunctionCall != nil:
		call := part.FunctionCall
		id := call.ID
		if id == "" {
			s.calls++
			id = fmt.Sprintf("%s%d", generatedIDPrefix, s.calls)
		}
		args, err := json.Marshal(call.Args)
		if err != nil || call.Args == nil {
			args = nil
		}
		if !s.b.ToolCallStart(id, call.Name) {
			return false
		}
		s.last = s.b.CurrentIndex()
		s.b.SignOpenBlock(sig)
		return s.b.ToolCallEnd(args)

	case part.InlineData != nil:
		s.last++
		return s.b.ImageSigned(part.InlineData.MIMEType, part.InlineData.Data, sig)

	case part.Text != "" && part.Thought:
		if !s.b.ThinkingDelta(part.Text) {
			return false
		}
		s.last = s.b.CurrentIndex()

	case part.Text != "":
		if !s.b.TextDelta(part.Text) {
			return false
		}
		s.last = s.b.CurrentIndex()
	}
	if sig != "" {
		s.b.SetSignature(s.last, sig)
	}
	return true
}

// finish emits the terminal event after the stream was read to the end.
func (s *streamState) finish() {
	switch s.finishReason {
	case genai.FinishReasonStop:
		s.b.Done(chat.StopReasonStop)
	case genai.FinishReasonMaxTokens:
		s.b.Done(chat.StopReasonLength)
	case "":
		if s.blockReason != "" {
			s.b.Fail(&chat.Error{Kind: chat.KindRefusal, Message: "gemini: request blocked: " + string(s.blockReason)})
			return
		}
		s.b.Fail(&chat.Error{Kind: chat.KindNetwork, Message: "gemini: stream ended before a finishReason"})
	case genai.FinishReasonMalformedFunctionCall, genai.FinishReasonUnexpectedToolCall, genai.FinishReasonOther:
		// The model produced invalid output this time (3.1 pro occasionally returns MALFORMED_FUNCTION_CALL in testing); a retry usually recovers.
		s.b.Fail(&chat.Error{Kind: chat.KindServer, Message: s.failureText("response ended abnormally")})
	default:
		// SAFETY, RECITATION, BLOCKLIST, PROHIBITED_CONTENT, SPII and the various image safety reasons.
		s.b.Fail(&chat.Error{Kind: chat.KindRefusal, Message: s.failureText("response blocked")})
	}
}

func (s *streamState) failureText(prefix string) string {
	text := "gemini: " + prefix + ": " + string(s.finishReason)
	if s.finishMessage != "" {
		text += ": " + s.finishMessage
	}
	return text
}
