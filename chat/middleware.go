package chat

import (
	"context"
	"iter"
)

// Middleware wraps a Client and returns a Client with extra behavior.
// Logging, usage recording, retries and falling back to another model are all implemented this way, and Chain stacks them.
type Middleware func(Client) Client

// Chain applies middlewares in order: the first is the outermost, seeing the request first and the events last.
func Chain(base Client, middlewares ...Middleware) Client {
	for i := len(middlewares) - 1; i >= 0; i-- {
		if middlewares[i] != nil {
			base = middlewares[i](base)
		}
	}
	return base
}

// StreamFunc is one model call, with the same signature as Client.Stream.
type StreamFunc func(ctx context.Context, req Request) iter.Seq[Event]

// WrapFunc wraps one model call: it can rewrite the request, observe or rewrite events, or call next zero or several times.
// The returned sequence must honor the Client.Stream contract. info is the model info of the wrapped Client.
type WrapFunc func(ctx context.Context, req Request, info ModelInfo, next StreamFunc) iter.Seq[Event]

// Wrap builds a middleware from a wrap function.
func Wrap(fn WrapFunc) Middleware {
	return func(base Client) Client {
		return wrappedClient{base: base, fn: fn}
	}
}

type wrappedClient struct {
	base Client
	fn   WrapFunc
}

func (c wrappedClient) Info() ModelInfo { return c.base.Info() }

func (c wrappedClient) Stream(ctx context.Context, req Request) iter.Seq[Event] {
	return c.fn(ctx, req, c.base.Info(), c.base.Stream)
}

// Hooks are callbacks for observing or slightly adjusting requests, a simplified form of Wrap. Every field may be nil.
type Hooks struct {
	// BeforeRequest is called before the request is sent and may modify it (slices in the request are shared with the caller;
	// copy them before changing messages). Returning an error aborts the call and the stream ends with an error event.
	BeforeRequest func(ctx context.Context, req *Request) error
	// OnEvent is called for every event. Event.Message is only valid during the callback.
	OnEvent func(ctx context.Context, event Event)
	// AfterResponse is called after the stream ends with the final message, also on failure (including the partial content).
	// It is not called when the caller stops iterating early.
	AfterResponse func(ctx context.Context, req Request, msg *Message)
}

// WithHooks turns Hooks into a middleware.
func WithHooks(h Hooks) Middleware {
	return Wrap(func(ctx context.Context, req Request, info ModelInfo, next StreamFunc) iter.Seq[Event] {
		return func(yield func(Event) bool) {
			if h.BeforeRequest != nil {
				if err := h.BeforeRequest(ctx, &req); err != nil {
					if req.Model != "" {
						info.Model = req.Model
					}
					NewBuilder(yield, info).Fail(err)
					return
				}
			}
			for event := range next(ctx, req) {
				if h.OnEvent != nil {
					h.OnEvent(ctx, event)
				}
				terminal := event.Type == EventDone || event.Type == EventError
				if terminal && h.AfterResponse != nil {
					h.AfterResponse(ctx, req, event.Message)
				}
				if !yield(event) {
					return
				}
			}
		}
	})
}
