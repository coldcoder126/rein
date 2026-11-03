package chat_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
)

func tag(name string, log *[]string) chat.Middleware {
	return chat.WithHooks(chat.Hooks{
		BeforeRequest: func(_ context.Context, req *chat.Request) error {
			*log = append(*log, name+":before")
			req.SystemPrompt += name
			return nil
		},
		AfterResponse: func(context.Context, chat.Request, *chat.Message) { *log = append(*log, name+":after") },
	})
}

func TestChainOrderAndHooks(t *testing.T) {
	base := chattest.NewClient(chattest.Say("你好"))
	var log []string
	var deltas []string
	client := chat.Chain(base, tag("a", &log), nil, tag("b", &log), chat.WithHooks(chat.Hooks{
		OnEvent: func(_ context.Context, e chat.Event) {
			if e.Type == chat.EventTextDelta {
				deltas = append(deltas, e.Delta)
			}
		},
	}))
	events := chattest.Record(client.Stream(context.Background(), chat.Request{}))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ","); got != "a:before,b:before,b:after,a:after" {
		t.Fatalf("the first middleware should be outermost: %s", got)
	}
	if base.Requests()[0].SystemPrompt != "ab" || strings.Join(deltas, "") != "你好" {
		t.Fatalf("wrong request rewrite or event observation: %q %v", base.Requests()[0].SystemPrompt, deltas)
	}
	if client.Info().Model != base.Info().Model || client.Info().Provider != base.Info().Provider {
		t.Fatal("the wrapper should keep the model info")
	}
}

func TestHooksBeforeRequestErrorAbortsCall(t *testing.T) {
	base := chattest.NewClient(chattest.Say("不应到达"))
	client := chat.Chain(base, chat.WithHooks(chat.Hooks{
		BeforeRequest: func(context.Context, *chat.Request) error { return errors.New("被策略拦截") },
	}))
	events := chattest.Record(client.Stream(context.Background(), chat.Request{}))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	final := events[len(events)-1]
	if final.Type != chat.EventError || !strings.Contains(final.Err.Error(), "被策略拦截") || len(base.Requests()) != 0 {
		t.Fatalf("should end with error without calling the model: %+v", final)
	}
}

// A wrapping middleware can call next several times; retries and fallbacks build on this.
func TestWrapCanRetry(t *testing.T) {
	base := chattest.NewClient(chattest.Fail(&chat.Error{Kind: chat.KindServer, Message: "过载"}), chattest.Say("成功"))
	retry := chat.Wrap(func(ctx context.Context, req chat.Request, _ chat.ModelInfo, next chat.StreamFunc) iter.Seq[chat.Event] {
		return func(yield func(chat.Event) bool) {
			for attempt := 0; ; attempt++ {
				var failed bool
				for e := range next(ctx, req) {
					if e.Type == chat.EventError && attempt == 0 && chat.AsError(e.Err).Retryable() {
						failed = true
						break
					}
					// Simplified: events of the first attempt are not forwarded.
					if attempt > 0 && !yield(e) {
						return
					}
				}
				if !failed {
					return
				}
			}
		}
	})
	msg, err := chat.Complete(context.Background(), chat.Chain(base, retry), chat.Request{})
	if err != nil || msg.TextContent() != "成功" || len(base.Requests()) != 2 {
		t.Fatalf("the second attempt should succeed: %v %+v", err, msg)
	}
}

func TestHTTPHooks(t *testing.T) {
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		w.Header().Set("X-Ratelimit-Remaining", "9")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	var seen string
	var status int
	var remaining string
	hooks := chat.HTTPHooks{
		OnPayload: func(_ context.Context, body []byte) []byte {
			seen = string(body)
			return bytes.Replace(body, []byte("原始"), []byte("改写"), 1)
		},
		OnResponse: func(_ context.Context, s int, h http.Header) { status, remaining = s, h.Get("X-Ratelimit-Remaining") },
	}
	resp, err := hooks.HTTPClient(nil).Post(server.URL, "text/plain", strings.NewReader("原始请求体"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen != "原始请求体" || received != "改写请求体" || status != http.StatusAccepted || remaining != "9" {
		t.Fatalf("wrong hook behavior: seen=%q received=%q status=%d remaining=%q", seen, received, status, remaining)
	}
	base := &http.Client{}
	if (chat.HTTPHooks{}).HTTPClient(base) != base {
		t.Fatal("should return base unchanged without hooks")
	}
}
