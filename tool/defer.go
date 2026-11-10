package tool

import (
	"context"
	"encoding/json"
	"time"

	"github.com/coldcoder126/rein/chat"
)

// Defer marks a tool as deferred (see chat.ToolDef.Deferred): the model does not see it until it finds it through the
// provider's tool search. Use it for tools that are rarely needed; keep the few tools used in most turns loaded.
// The optional interfaces Sequential, Timeouter and NeedsTaskManager are passed through.
func Defer(t Tool) Tool {
	if t == nil {
		return nil
	}
	return &deferred{Tool: t}
}

// DeferAll marks every tool of a toolset as deferred, see Defer. Its Instructor, if any, is passed through.
func DeferAll(set Toolset) Toolset {
	if set == nil {
		return nil
	}
	return &deferredSet{set: set}
}

type deferred struct {
	Tool
}

func (d *deferred) Def() chat.ToolDef {
	def := d.Tool.Def()
	def.Deferred = true
	return def
}

func (d *deferred) Execute(ctx *Context, args json.RawMessage) (*Result, error) {
	return d.Tool.Execute(ctx, args)
}

// Unwrap returns the tool that was deferred.
func (d *deferred) Unwrap() Tool { return d.Tool }

func (d *deferred) Sequential() bool {
	s, ok := d.Tool.(Sequential)
	return ok && s.Sequential()
}

func (d *deferred) Timeout() time.Duration {
	if t, ok := d.Tool.(Timeouter); ok {
		return t.Timeout()
	}
	return 0
}

func (d *deferred) NeedsTaskManager() bool {
	n, ok := d.Tool.(NeedsTaskManager)
	return ok && n.NeedsTaskManager()
}

type deferredSet struct {
	set Toolset
}

func (s *deferredSet) Tools(ctx context.Context, info RunInfo) ([]Tool, error) {
	tools, err := s.set.Tools(ctx, info)
	if err != nil {
		return nil, err
	}
	out := make([]Tool, len(tools))
	for i, t := range tools {
		out[i] = Defer(t)
	}
	return out, nil
}

func (s *deferredSet) Instructions(ctx context.Context, info RunInfo) (string, error) {
	if in, ok := s.set.(Instructor); ok {
		return in.Instructions(ctx, info)
	}
	return "", nil
}
