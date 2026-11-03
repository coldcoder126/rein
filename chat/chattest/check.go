package chattest

import (
	"fmt"
	"iter"

	"github.com/coldcoder126/rein/chat"
)

// Record consumes an event sequence and returns all events. The Message in an event is a live object,
// so only the events themselves are kept; the final message comes from the terminal event's Message.
func Record(events iter.Seq[chat.Event]) []chat.Event {
	var out []chat.Event
	for e := range events {
		out = append(out, e)
	}
	return out
}

// CheckStream checks that an event sequence honors the chat.Client stream contract, for provider contract tests.
func CheckStream(events []chat.Event) error {
	if len(events) == 0 {
		return fmt.Errorf("empty event sequence")
	}
	if events[0].Type != chat.EventStart {
		return fmt.Errorf("first event should be start, got=%s", events[0].Type)
	}

	openType := chat.EventType("")
	openIndex := -1
	nextIndex := 0
	for i, e := range events {
		if e.Message == nil {
			return fmt.Errorf("event %d(%s) has no Message", i, e.Type)
		}
		terminal := e.Type == chat.EventDone || e.Type == chat.EventError
		if i == len(events)-1 && !terminal {
			return fmt.Errorf("last event should be done or error, got=%s", e.Type)
		}
		if i < len(events)-1 && terminal {
			return fmt.Errorf("events after terminal event %s", e.Type)
		}
		if i > 0 && e.Type == chat.EventStart {
			return fmt.Errorf("duplicate start event")
		}

		switch e.Type {
		case chat.EventTextStart, chat.EventThinkingStart, chat.EventToolCallStart:
			if openType != "" {
				return fmt.Errorf("event %d: new block opened while block %d is still open", i, openIndex)
			}
			if e.Index != nextIndex {
				return fmt.Errorf("event %d: block index should be %d, got=%d", i, nextIndex, e.Index)
			}
			if e.Type == chat.EventToolCallStart && (e.ToolCall == nil || e.ToolCall.Name == "") {
				return fmt.Errorf("event %d: tool_call_start has no tool name", i)
			}
			openType, openIndex = e.Type, e.Index
			nextIndex++
		case chat.EventImage:
			if openType != "" {
				return fmt.Errorf("event %d: image while block %d is still open", i, openIndex)
			}
			if e.Index != nextIndex {
				return fmt.Errorf("event %d: block index should be %d, got=%d", i, nextIndex, e.Index)
			}
			if e.Index >= len(e.Message.Content) || e.Message.Content[e.Index].Type != chat.ContentTypeMedia {
				return fmt.Errorf("event %d: the block of the image event is not an image", i)
			}
			nextIndex++
		case chat.EventTextDelta:
			if err := checkOpen(i, e, openType, openIndex, chat.EventTextStart); err != nil {
				return err
			}
		case chat.EventThinkingDelta:
			if err := checkOpen(i, e, openType, openIndex, chat.EventThinkingStart); err != nil {
				return err
			}
		case chat.EventTextEnd:
			if err := checkOpen(i, e, openType, openIndex, chat.EventTextStart); err != nil {
				return err
			}
			openType = ""
		case chat.EventThinkingEnd:
			if err := checkOpen(i, e, openType, openIndex, chat.EventThinkingStart); err != nil {
				return err
			}
			openType = ""
		case chat.EventToolCallEnd:
			if err := checkOpen(i, e, openType, openIndex, chat.EventToolCallStart); err != nil {
				return err
			}
			if e.ToolCall == nil || len(e.ToolCall.Arguments) == 0 {
				return fmt.Errorf("event %d: tool_call_end has no arguments", i)
			}
			openType = ""
		case chat.EventDone:
			if e.Err != nil {
				return fmt.Errorf("done event should not carry Err")
			}
		case chat.EventError:
			if e.Err == nil {
				return fmt.Errorf("error event has no Err")
			}
			if e.Reason != chat.StopReasonError && e.Reason != chat.StopReasonAborted {
				return fmt.Errorf("invalid Reason on error event: %s", e.Reason)
			}
		}
		if terminal && openType != "" {
			// On failure an open tool call block has incomplete arguments and is dropped without an end event.
			if e.Type == chat.EventError && openType == chat.EventToolCallStart {
				nextIndex--
				openType = ""
			} else {
				return fmt.Errorf("block %d is still open before the terminal event", openIndex)
			}
		}
	}

	final := events[len(events)-1].Message
	if len(final.Content) != nextIndex {
		return fmt.Errorf("final message has %d content blocks but the events have %d", len(final.Content), nextIndex)
	}
	for i, e := range events {
		if e.Type == chat.EventTextEnd || e.Type == chat.EventThinkingEnd {
			if e.Content != final.Content[e.Index].Text {
				return fmt.Errorf("event %d: Content of %s differs from the final message", i, e.Type)
			}
		}
	}
	return nil
}

func checkOpen(i int, e chat.Event, openType chat.EventType, openIndex int, want chat.EventType) error {
	if openType != want || openIndex != e.Index {
		return fmt.Errorf("event %d: %s has no matching open block (open=%s#%d)", i, e.Type, openType, openIndex)
	}
	return nil
}
