package chat_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/coldcoder126/rein/chat"
)

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
		want    chat.ErrorKind
	}{
		{"auth failure", 401, "invalid x-api-key", chat.KindAuth},
		{"rate limit", 429, "rate_limit_error", chat.KindRateLimit},
		{"exhausted quota is not a rate limit", 429, "You exceeded your current quota: insufficient_quota", chat.KindQuota},
		{"server overloaded", 529, "overloaded_error", chat.KindServer},
		{"gateway error", 502, "bad gateway", chat.KindServer},
		{"request too large", 413, "request_too_large", chat.KindContextLength},
		{"Anthropic overflow", 400, "prompt is too long: 213462 tokens > 200000 maximum", chat.KindContextLength},
		{"plain invalid parameter", 400, "messages: field required", chat.KindInvalidRequest},
		{"rate limit text without status", 0, "rate limit exceeded", chat.KindRateLimit},
		{"overload event without status", 0, `{"type":"overloaded_error"}`, chat.KindServer},
		{"connection drop without status", 0, "stream ended before message_stop", chat.KindNetwork},
		{"unknown", 0, "something odd", chat.KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chat.ClassifyStatus(tt.status, tt.message); got != tt.want {
				t.Fatalf("got=%s want=%s", got, tt.want)
			}
		})
	}
}

func TestIsContextOverflowExcludesThrottling(t *testing.T) {
	if !chat.IsContextOverflow("Input length (265330) exceeds model's maximum context length (262144).") {
		t.Fatal("should recognize the overflow message of OpenAI-compatible endpoints")
	}
	if chat.IsContextOverflow("ThrottlingException: Too many tokens, please wait before trying again.") {
		t.Fatal("rate limit text should not be mistaken for overflow")
	}
}

func TestErrorSentinelsAndRetryable(t *testing.T) {
	err := error(&chat.Error{Kind: chat.KindContextLength, Message: "x"})
	if !errors.Is(err, chat.ErrContextLengthExceeded) || errors.Is(err, chat.ErrRateLimited) {
		t.Fatal("errors.Is should match sentinel errors by kind")
	}
	for kind, want := range map[chat.ErrorKind]bool{
		chat.KindRateLimit: true, chat.KindServer: true, chat.KindNetwork: true,
		chat.KindQuota: false, chat.KindAuth: false, chat.KindContextLength: false,
		chat.KindInvalidRequest: false, chat.KindRefusal: false, chat.KindAborted: false,
	} {
		if got := kind.Retryable(); got != want {
			t.Fatalf("%s Retryable=%v want=%v", kind, got, want)
		}
	}
}

func TestWrapError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ce := chat.WrapError(ctx, io.ErrUnexpectedEOF, 0, 0); ce.Kind != chat.KindAborted {
		t.Fatalf("a canceled ctx should be aborted: %s", ce.Kind)
	}
	if ce := chat.WrapError(context.Background(), io.ErrUnexpectedEOF, 0, 0); ce.Kind != chat.KindNetwork {
		t.Fatalf("unexpected EOF should be network: %s", ce.Kind)
	}
	ce := chat.WrapError(context.Background(), errors.New("boom"), 503, 2*time.Second)
	if ce.Kind != chat.KindServer || ce.Status != 503 || ce.RetryAfter != 2*time.Second {
		t.Fatalf("status code lost: %+v", ce)
	}
	inner := &chat.Error{Kind: chat.KindAuth}
	if got := chat.WrapError(context.Background(), inner, 0, 0); got != inner {
		t.Fatal("an *Error should be returned unchanged")
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("retry-after-ms", "1500")
	if got := chat.ParseRetryAfter(h); got != 1500*time.Millisecond {
		t.Fatalf("wrong retry-after-ms parsing: %s", got)
	}
	h = http.Header{}
	h.Set("retry-after", "3")
	if got := chat.ParseRetryAfter(h); got != 3*time.Second {
		t.Fatalf("wrong retry-after seconds parsing: %s", got)
	}
	if got := chat.ParseRetryAfter(nil); got != 0 {
		t.Fatalf("nil header should return 0: %s", got)
	}
}
