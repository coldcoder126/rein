// Package chattest provides a scripted Client for testing users of chat.Client, plus a stream contract checker.
package chattest

import (
	"context"
	"encoding/json"
	"iter"
	"sync"

	"github.com/coldcoder126/rein/chat"
)

// Script describes what one model call produces. If the script returns without a terminal event, Done is emitted.
type Script func(ctx context.Context, b *chat.Builder)

// Say produces some text and finishes normally.
func Say(text string) Script {
	return func(_ context.Context, b *chat.Builder) {
		b.TextDelta(text)
		b.Done(chat.StopReasonStop)
	}
}

// CallTools produces tool calls and finishes with tool_use.
func CallTools(calls ...chat.ToolCall) Script {
	return SayAndCall("", calls...)
}

// SayAndCall produces text followed by tool calls.
func SayAndCall(text string, calls ...chat.ToolCall) Script {
	return func(_ context.Context, b *chat.Builder) {
		b.TextDelta(text)
		for _, call := range calls {
			args := call.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			b.ToolCall(call.ID, call.Name, args)
		}
		b.Done(chat.StopReasonToolUse)
	}
}

// Fail produces a failing terminal event.
func Fail(err error) Script {
	return func(ctx context.Context, b *chat.Builder) {
		b.Fail(chat.WrapError(ctx, err, 0, 0))
	}
}

// Gate notifies started before running next, then waits for release or for ctx to end; when ctx ends it fails as canceled.
func Gate(started chan<- struct{}, release <-chan struct{}, next Script) Script {
	return func(ctx context.Context, b *chat.Builder) {
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				b.Fail(chat.WrapError(ctx, ctx.Err(), 0, 0))
				return
			}
		}
		next(ctx, b)
	}
}

// Client is a chat.Client that runs scripts in order and records the requests it receives.
type Client struct {
	Model   chat.ModelInfo
	Scripts []Script

	mu       sync.Mutex
	requests []chat.Request
}

// NewClient creates a Client that runs scripts in order. Once the scripts run out, later calls produce an empty reply.
func NewClient(scripts ...Script) *Client {
	return &Client{
		Model:   chat.ModelInfo{Provider: "stub", Model: "stub"},
		Scripts: scripts,
	}
}

// Requests returns a copy of the requests received so far.
func (c *Client) Requests() []chat.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]chat.Request(nil), c.requests...)
}

func (c *Client) Info() chat.ModelInfo { return c.Model }

func (c *Client) Stream(ctx context.Context, req chat.Request) iter.Seq[chat.Event] {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	index := len(c.requests) - 1
	var script Script
	if index < len(c.Scripts) {
		script = c.Scripts[index]
	}
	c.mu.Unlock()

	return func(yield func(chat.Event) bool) {
		b := chat.NewBuilder(yield, c.Model.Provider, c.Model.Model)
		if script != nil {
			script(ctx, b)
		}
		b.Done("")
	}
}
