package agent

import (
	"context"
	"iter"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

// Middleware is a set of hooks wrapping an Agent run; every field may be nil. From outside in, the four hooks wrap
// a run, each of its turns, and the model call and tool executions within a turn; each layer gets next and decides whether and how to continue.
// One concern (tracing, auditing, human approval) can be packaged as one Middleware.
//
// Middlewares form a chain of responsibility with the first being outermost, which also decides the order of the same hook across layers.
type Middleware struct {
	// WrapRun wraps a run, covering the resolution of the system prompt and tools as well as every turn.
	WrapRun func(ctx context.Context, info tool.RunInfo, next RunNext) (*Result, error)
	// WrapTurn wraps a turn: injecting pending messages, calling the model, executing tools. See TurnResult.
	// next may be called at most once; when it is not called the turn does not run and the returned result goes straight to the outer layer.
	// Before calling next, SetMessages can replace the history the turn uses.
	WrapTurn func(ctx context.Context, turn TurnInfo, next TurnNext) (*TurnResult, error)
	// WrapModel wraps a model call: it can rewrite the request, observe or rewrite events, or call next zero or several times.
	// The returned sequence must honor the chat.Client.Stream contract. req.Messages is a copy that can be changed directly.
	// The tools callable in this turn are those in the request of the last call to next; tools not offered are not executed even if called.
	// Extensions at the model call level that are unrelated to the run (retries, rate limiting, usage) wrap Model with chat.Chain.
	// It also wraps side calls (turn.SideCall), whose requests should pass through unchanged; see SideCall.
	WrapModel func(ctx context.Context, turn TurnInfo, req chat.Request, next chat.StreamFunc) iter.Seq[chat.Event]
	// WrapTool wraps a tool execution; see WrapToolFunc.
	WrapTool WrapToolFunc
}

// RunNext continues the run.
type RunNext func(ctx context.Context) (*Result, error)

// TurnNext runs the turn and returns its result.
type TurnNext func(ctx context.Context) (*TurnResult, error)

// BeforeModel builds a middleware that only modifies the request before the model call. req.Messages is a copy that can be changed directly,
// and tools removed from req.Tools cannot be called in this turn. Returning an error fails the model call, and the run ends with StatusFailed.
func BeforeModel(fn func(ctx context.Context, turn TurnInfo, req *chat.Request) error) Middleware {
	return Middleware{WrapModel: func(ctx context.Context, turn TurnInfo, req chat.Request, next chat.StreamFunc) iter.Seq[chat.Event] {
		if err := fn(ctx, turn, &req); err != nil {
			return func(yield func(chat.Event) bool) {
				chat.NewBuilder(yield, chat.ModelInfo{Model: req.Model}).Fail(err)
			}
		}
		return next(ctx, req)
	}}
}

// The functions below chain the same hook of every layer, the first middleware being outermost.

func chainRun(ms []Middleware, info tool.RunInfo, next RunNext) RunNext {
	for i := len(ms) - 1; i >= 0; i-- {
		if wrap, inner := ms[i].WrapRun, next; wrap != nil {
			next = func(ctx context.Context) (*Result, error) { return wrap(ctx, info, inner) }
		}
	}
	return next
}

func chainTurn(ms []Middleware, turn TurnInfo, next TurnNext) TurnNext {
	for i := len(ms) - 1; i >= 0; i-- {
		if wrap, inner := ms[i].WrapTurn, next; wrap != nil {
			next = func(ctx context.Context) (*TurnResult, error) { return wrap(ctx, turn, inner) }
		}
	}
	return next
}

func chainModel(ms []Middleware, turn TurnInfo, next chat.StreamFunc) chat.StreamFunc {
	for i := len(ms) - 1; i >= 0; i-- {
		if wrap, inner := ms[i].WrapModel, next; wrap != nil {
			next = func(ctx context.Context, req chat.Request) iter.Seq[chat.Event] { return wrap(ctx, turn, req, inner) }
		}
	}
	return next
}

// chainTool turns each layer's error or nil result into a result in place, so outer layers always see a result.
func chainTool(ms []Middleware, next ToolNext) ToolNext {
	for i := len(ms) - 1; i >= 0; i-- {
		if wrap, inner := ms[i].WrapTool, next; wrap != nil {
			next = func(ctx context.Context, inv *ToolInvocation) *tool.Result {
				result, err := wrap(ctx, inv, inner)
				if err != nil {
					return errorResult(err.Error())
				}
				if result == nil {
					return &tool.Result{Content: []chat.ContentBlock{chat.Text("")}}
				}
				return result
			}
		}
	}
	return next
}
