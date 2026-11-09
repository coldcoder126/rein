package agent

import (
	"context"
	"errors"
	"slices"

	"github.com/coldcoder126/rein/chat"
)

// SideCallOptions adjusts a side call. Only parameters that do not affect prompt caching can be changed.
type SideCallOptions struct {
	// MaxTokens limits the output of the side call; 0 keeps Agent.Options.MaxTokens.
	MaxTokens int
}

// SideCall makes a side call within the current run: a model call that is not a turn, made for a middleware's own purpose
// (such as summarizing history). ctx must come from a hook of the run.
//
// The request is built exactly like a turn's, from the system prompt, tools and options resolved for the run, with messages
// in place of the history, and it goes through every WrapModel with TurnInfo.SideCall set. Keeping the request identical
// is what lets the side call reuse the conversation's prompt cache, so middlewares should not change side call requests.
// Tool calls in the reply are not executed; the reply does not enter the history, emits no events and does not count toward MaxTurns.
//
// It returns the assistant message, and a *chat.Error when the model call fails, like chat.Complete.
func SideCall(ctx context.Context, messages []chat.Message, opts SideCallOptions) (*chat.Message, error) {
	l, _ := ctx.Value(loopKey{}).(*loop)
	if l == nil {
		return nil, errors.New("agent: SideCall must be called within a run")
	}
	return l.sideCall(ctx, messages, opts)
}

// SideCall makes a side call outside a run, for upper layers that need one while no run is in progress.
// It is a run of its own with a single model call: WrapRun, the resolution of the system prompt and tools, and WrapModel
// see it as they would see a run, and in.Messages take the place of the history. See the package function SideCall.
func (a *Agent) SideCall(ctx context.Context, in Input, opts SideCallOptions) (*chat.Message, error) {
	l, err := a.newLoop(ctx, in)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, loopKey{}, l)
	var msg *chat.Message
	_, err = chainRun(l.middlewares, l.in.RunInfo, func(ctx context.Context) (*Result, error) {
		if err := l.resolve(ctx); err != nil {
			return nil, err
		}
		var err error
		msg, err = l.sideCall(ctx, l.messages, opts)
		if msg != nil {
			l.result.Usage = msg.Usage
			l.result.Final = msg
		}
		l.result.Status = StatusCompleted
		if err != nil {
			l.result.Status = StatusFailed
		}
		return l.result, err
	})(ctx)
	return msg, err
}

// SetMessages replaces the history that the coming turn and later turns use, such as after compacting it.
// It may only be called from WrapTurn before calling next; Result.Messages is not affected.
func SetMessages(ctx context.Context, messages []chat.Message) error {
	l, _ := ctx.Value(loopKey{}).(*loop)
	switch {
	case l == nil || !l.beforeTurn.Load():
		return errors.New("agent: SetMessages must be called from WrapTurn before next")
	case len(messages) == 0:
		return errors.New("agent: SetMessages needs at least one message")
	case messages[len(messages)-1].Role == chat.RoleAssistant:
		return errors.New("agent: messages must not end with an assistant message")
	}
	l.messages = slices.Clone(messages)
	return nil
}

func (l *loop) sideCall(ctx context.Context, messages []chat.Message, opts SideCallOptions) (*chat.Message, error) {
	info := TurnInfo{RunInfo: l.in.RunInfo, TurnIndex: l.result.Turns, SideCall: true}
	req := l.request(info.TurnIndex, messages)
	if opts.MaxTokens > 0 {
		req.MaxTokens = opts.MaxTokens
	}
	return chat.Collect(chainModel(l.middlewares, info, l.agent.Model.Stream)(ctx, req))
}
