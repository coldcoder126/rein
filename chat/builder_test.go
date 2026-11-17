package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
)

func record(fn func(b *chat.Builder)) []chat.Event {
	return chattest.Record(func(yield func(chat.Event) bool) {
		fn(chat.NewBuilder(yield, chat.ModelInfo{Provider: "test", Model: "m1"}))
	})
}

func TestBuilderInterleavedBlocksKeepOrder(t *testing.T) {
	events := record(func(b *chat.Builder) {
		b.ThinkingDelta("想一想")
		b.ThinkingSignature("sig")
		b.TextDelta("先说")
		b.TextDelta("两句")
		b.ThinkingDelta("再想")
		b.ToolCall("call_1", "lookup", json.RawMessage(`{"q":"a"}`))
		b.TextDelta("收尾")
		b.SetUsage(chat.Usage{Input: 3, Output: 4, TotalTokens: 7})
		b.Done("")
	})
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}

	final := events[len(events)-1]
	if final.Type != chat.EventDone || final.Reason != chat.StopReasonToolUse {
		t.Fatalf("wrong terminal event: %+v", final)
	}
	msg := final.Message
	wantTypes := []chat.ContentType{
		chat.ContentTypeThinking, chat.ContentTypeText, chat.ContentTypeThinking,
		chat.ContentTypeToolCall, chat.ContentTypeText,
	}
	if len(msg.Content) != len(wantTypes) {
		t.Fatalf("wrong number of content blocks: %d", len(msg.Content))
	}
	for i, want := range wantTypes {
		if msg.Content[i].Type != want {
			t.Fatalf("block %d has the wrong type: got=%s want=%s", i, msg.Content[i].Type, want)
		}
	}
	if msg.Content[0].Text != "想一想" || msg.Content[0].Signature != "sig" || msg.Content[1].Text != "先说两句" {
		t.Fatalf("wrong block content: %+v", msg.Content)
	}
	if msg.Provider != "test" || msg.Model != "m1" || msg.Usage.TotalTokens != 7 {
		t.Fatalf("wrong message metadata: %+v", msg)
	}
}

func TestBuilderDoneInfersStopReason(t *testing.T) {
	events := record(func(b *chat.Builder) {
		b.TextDelta("你好")
		b.Done("")
	})
	if got := events[len(events)-1].Reason; got != chat.StopReasonStop {
		t.Fatalf("no tool calls should infer stop, got=%s", got)
	}

	events = record(func(b *chat.Builder) {
		b.ToolCall("c1", "lookup", nil)
		b.Done(chat.StopReasonStop)
	})
	final := events[len(events)-1]
	if final.Reason != chat.StopReasonToolUse {
		t.Fatalf("stop should become tool_use when there are tool calls, got=%s", final.Reason)
	}
	if string(final.Message.Content[0].ToolCall.Arguments) != "{}" {
		t.Fatalf("empty arguments should become {}: %s", final.Message.Content[0].ToolCall.Arguments)
	}
}

func TestBuilderFailKeepsPartialAndDropsUnfinishedToolCall(t *testing.T) {
	events := record(func(b *chat.Builder) {
		b.TextDelta("已生成")
		b.ToolCallStart("c1", "lookup")
		b.Fail(&chat.Error{Kind: chat.KindNetwork, Message: "连接中断"})
	})
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	final := events[len(events)-1]
	if final.Type != chat.EventError || final.Reason != chat.StopReasonError {
		t.Fatalf("wrong terminal event: %+v", final)
	}
	msg := final.Message
	if len(msg.Content) != 1 || msg.Content[0].Text != "已生成" {
		t.Fatalf("should keep generated content and drop the unfinished tool call: %+v", msg.Content)
	}
	if msg.StopReason != chat.StopReasonError || msg.ErrorKind != chat.KindNetwork || msg.ErrorMessage == "" {
		t.Fatalf("failure not recorded on the message: %+v", msg)
	}
	var ce *chat.Error
	if !errors.As(final.Err, &ce) || !ce.Retryable() {
		t.Fatalf("Err should be a retryable *chat.Error: %v", final.Err)
	}
}

func TestBuilderFailWithoutContentStillStarts(t *testing.T) {
	events := record(func(b *chat.Builder) {
		b.Fail(&chat.Error{Kind: chat.KindAuth, Message: "无权限"})
	})
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != chat.EventStart || events[1].Type != chat.EventError {
		t.Fatalf("start should come before the failure: %+v", events)
	}
}

func TestBuilderCanceledBecomesAborted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events := record(func(b *chat.Builder) {
		b.TextDelta("半截")
		b.Fail(chat.WrapError(ctx, ctx.Err(), 0, 0))
	})
	final := events[len(events)-1]
	if final.Reason != chat.StopReasonAborted || final.Message.ErrorKind != chat.KindAborted {
		t.Fatalf("cancellation should be aborted: %+v", final)
	}
	if !errors.Is(final.Err, chat.ErrAborted) {
		t.Fatalf("Err should match ErrAborted: %v", final.Err)
	}
}

func TestBuilderRedactedThinking(t *testing.T) {
	events := record(func(b *chat.Builder) {
		b.RedactedThinking("encrypted")
		b.TextDelta("答案")
		b.Done("")
	})
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	block := events[len(events)-1].Message.Content[0]
	if block.Type != chat.ContentTypeThinking || !block.Redacted || block.Signature != "encrypted" {
		t.Fatalf("wrong redacted thinking block: %+v", block)
	}
}

func TestBuilderStopsWhenConsumerBreaks(t *testing.T) {
	var returned []bool
	for range func(yield func(chat.Event) bool) {
		b := chat.NewBuilder(yield, chat.ModelInfo{Provider: "test", Model: "m1"})
		returned = append(returned, b.TextDelta("a"))
		returned = append(returned, b.TextDelta("b"))
		returned = append(returned, b.Done(""))
	} {
		break
	}
	for i, ok := range returned {
		if ok {
			t.Fatalf("call %d after the caller stopped should return false", i)
		}
	}
}

func TestCompleteReturnsFinalMessageOrPartialWithError(t *testing.T) {
	client := chattest.NewClient(chattest.Say("完成"), chattest.Fail(errors.New("boom")))

	msg, err := chat.Complete(context.Background(), client, chat.Request{})
	if err != nil || msg.TextContent() != "完成" || msg.StopReason != chat.StopReasonStop {
		t.Fatalf("Complete success path: msg=%+v err=%v", msg, err)
	}

	msg, err = chat.Complete(context.Background(), client, chat.Request{})
	if err == nil || msg == nil || msg.StopReason != chat.StopReasonError {
		t.Fatalf("Complete failure path should return the partial message and an error: msg=%+v err=%v", msg, err)
	}
}

func TestBuilderImageEventClosesBlockImmediately(t *testing.T) {
	events := record(func(b *chat.Builder) {
		b.TextDelta("先说明")
		b.Image("image/png", []byte{1, 2, 3})
		b.TextDelta("再补充")
		b.Done("")
	})
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	var types []chat.EventType
	for _, e := range events {
		types = append(types, e.Type)
	}
	want := []chat.EventType{
		chat.EventStart, chat.EventTextStart, chat.EventTextDelta, chat.EventTextEnd,
		chat.EventImage,
		chat.EventTextStart, chat.EventTextDelta, chat.EventTextEnd, chat.EventDone,
	}
	if len(types) != len(want) {
		t.Fatalf("wrong event sequence: %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("wrong event sequence: %v", types)
		}
	}
	msg := events[len(events)-1].Message
	if len(msg.Content) != 3 || msg.Content[1].Type != chat.ContentTypeMedia ||
		msg.Content[1].MIMEType != "image/png" || len(msg.Content[1].Data) != 3 {
		t.Fatalf("the image block should sit between the two text blocks: %+v", msg.Content)
	}
}

func TestBuilderImageAfterFailureKeepsGeneratedImage(t *testing.T) {
	events := record(func(b *chat.Builder) {
		b.Image("image/png", []byte{9})
		b.Fail(&chat.Error{Kind: chat.KindNetwork, Message: "断流"})
	})
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1].Message.Content; len(got) != 1 || got[0].Type != chat.ContentTypeMedia {
		t.Fatalf("generated images should be kept on failure: %+v", got)
	}
}
