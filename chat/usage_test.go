package chat_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
)

func usageClient(script chattest.Script, info chat.ModelInfo) *chattest.Client {
	client := chattest.NewClient(script)
	client.Model = info
	return client
}

func usageScript(usage chat.Usage, text string) chattest.Script {
	return func(_ context.Context, b *chat.Builder) {
		b.TextDelta(text)
		b.SetUsage(usage)
		b.Done("")
	}
}

func TestWithUsageRecorderStream(t *testing.T) {
	base := usageClient(usageScript(chat.Usage{Input: 10, Output: 3, CacheRead: 2, TotalTokens: 15}, "ok"),
		chat.ModelInfo{Provider: "openai", Model: "default-model"})
	var got chat.UsageEvent
	wrapped := chat.WithUsageRecorder(base, func(_ context.Context, event chat.UsageEvent) error {
		got = event
		return errors.New("recorder 失败不应影响主调用")
	}, chat.WithUsageProviderName("deepseek"))

	ctx := chat.WithUsageContext(context.Background(), chat.UsageContext{
		RequestID: "req-1", UserID: "user-1", SessionID: "session-1", TraceID: "trace-1", Source: "chat",
		Metadata: map[string]string{"run_id": "run-1", "shared": "ctx"},
	})
	msg, err := chat.Complete(ctx, wrapped, chat.Request{Options: chat.Options{
		Model:    "request-model",
		Metadata: map[string]string{"purpose": "answer", "shared": "req"},
	}})
	if err != nil || msg.TextContent() != "ok" {
		t.Fatalf("should not affect the main call: msg=%+v err=%v", msg, err)
	}
	if got.Provider != "deepseek" || got.Model != "request-model" || got.Method != "stream" || got.Error != "" {
		t.Fatalf("wrong basic usage fields: %+v", got)
	}
	if got.Usage.Input != 10 || got.Usage.Output != 3 || got.Usage.CacheRead != 2 || got.Usage.TotalTokens != 15 {
		t.Fatalf("wrong usage counts: %+v", got.Usage)
	}
	if got.RequestID != "req-1" || got.UserID != "user-1" || got.SessionID != "session-1" || got.TraceID != "trace-1" || got.Source != "chat" {
		t.Fatalf("wrong usage context: %+v", got)
	}
	if got.Metadata["run_id"] != "run-1" || got.Metadata["purpose"] != "answer" || got.Metadata["shared"] != "req" {
		t.Fatalf("wrong metadata merge: %+v", got.Metadata)
	}
}

func TestWithUsageRecorderRecordsFailure(t *testing.T) {
	base := usageClient(func(_ context.Context, b *chat.Builder) {
		b.TextDelta("半截")
		b.SetUsage(chat.Usage{Input: 5, TotalTokens: 5})
		b.Fail(&chat.Error{Kind: chat.KindServer, Message: "上游过载"})
	}, chat.ModelInfo{Provider: "anthropic", Model: "claude"})
	var got chat.UsageEvent
	wrapped := chat.WithUsageRecorder(base, func(_ context.Context, event chat.UsageEvent) error {
		got = event
		return nil
	})

	_, _ = chat.Complete(context.Background(), wrapped, chat.Request{})
	if !strings.Contains(got.Error, "上游过载") || got.Usage.TotalTokens != 5 {
		t.Fatalf("a failure should record the error and the usage so far: %+v", got)
	}
}

func TestWithUsageRecorderStreamStoppedBeforeCompletion(t *testing.T) {
	base := usageClient(usageScript(chat.Usage{Input: 1, Output: 1, TotalTokens: 2}, "ok"),
		chat.ModelInfo{Provider: "openai", Model: "gpt"})
	var got chat.UsageEvent
	wrapped := chat.WithUsageRecorder(base, func(_ context.Context, event chat.UsageEvent) error {
		got = event
		return nil
	})

	for range wrapped.Stream(context.Background(), chat.Request{}) {
		break
	}
	if !strings.Contains(got.Error, "stream stopped before completion") {
		t.Fatalf("expected an incomplete error to be recorded, got=%q", got.Error)
	}
}
