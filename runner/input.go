package runner

import (
	"errors"
	"fmt"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
	"github.com/coldcoder126/rein/tool"
)

// Input is one user input: content blocks in order, which can only be text and media (chat.Media, chat.MediaURL).
type Input struct {
	Content []chat.ContentBlock
}

// Text returns a plain text input.
func Text(text string) Input { return Input{Content: []chat.ContentBlock{chat.Text(text)}} }

// Parts returns an input made of several content blocks, such as some text and a few images.
func Parts(blocks ...chat.ContentBlock) Input { return Input{Content: blocks} }

// blocks validates and returns the input's content blocks. The content enters history as is: the framework does not restrict media types,
// download URLs, or convert or compress media; that is up to the caller (imageproc can process images).
func (in Input) blocks() ([]chat.ContentBlock, error) {
	var out []chat.ContentBlock
	for i, block := range in.Content {
		switch block.Type {
		case chat.ContentTypeText:
			if block.Text == "" {
				continue
			}
		case chat.ContentTypeMedia:
			if (len(block.Data) > 0) == (block.URL != "") {
				return nil, fmt.Errorf("runner: content block %d must have either inline data or a URL", i+1)
			}
		default:
			return nil, fmt.Errorf("runner: input does not support content block type %q", block.Type)
		}
		out = append(out, block)
	}
	if len(out) == 0 {
		return nil, errors.New("runner: input must not be empty")
	}
	return chat.CloneBlocks(out), nil
}

// ContextInput is business context injected with a run, such as the page the user is on. It is merged into the user message
// as <app_context>, visible to the model but hidden from the user by default.
type ContextInput struct {
	Text    string
	Content []chat.ContentBlock
	// Source and Kind describe where the context comes from and what it is, by the business layer's own convention, and may be empty; when set they become attributes of <app_context> visible to the model.
	Source string
	Kind   string
	// Scope defaults to session.ScopeRun: it only enters this run and is not written to the session.
	// session.ScopePersistent writes it to the session, keeping it visible to later runs.
	Scope   session.Scope
	Details any
	Visible bool
}

func (in ContextInput) message() session.Message {
	content := chat.CloneBlocks(in.Content)
	if len(content) == 0 && in.Text != "" {
		content = []chat.ContentBlock{chat.Text(in.Text)}
	}
	msg := session.ContextMessage(content, session.Message{
		Visible: in.Visible,
		Source:  in.Source,
		Kind:    in.Kind,
		Scope:   session.ScopeRun,
		Details: in.Details,
	})
	if in.Scope == session.ScopePersistent {
		msg.Scope = session.ScopePersistent
	}
	return msg
}

// RunOption adjusts a run.
type RunOption func(*runOptions)

type runOptions struct {
	context []ContextInput
	details any
	// A non-nil parent means this run is a sub-task, and its session hangs under the session of the run that started it.
	parent *tool.RunInfo
}

// WithContext injects business context into this run.
func WithContext(inputs ...ContextInput) RunOption {
	return func(o *runOptions) { o.context = append(o.context, inputs...) }
}

// WithDetails attaches business data to this run's user message (session.Message.Details).
func WithDetails(details any) RunOption {
	return func(o *runOptions) { o.details = details }
}
