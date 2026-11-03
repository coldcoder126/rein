package tool

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/coldcoder126/rein/chat"
)

// Option adjusts a tool created by New.
type Option func(*typed)

// RunSequentially requires the whole batch of calls containing this tool to run sequentially.
func RunSequentially() Option {
	return func(t *typed) { t.sequential = true }
}

// WithTimeout sets the tool's execution timeout, which takes precedence over the Agent's default.
func WithTimeout(d time.Duration) Option {
	return func(t *typed) { t.timeout = d }
}

// New creates a tool from a typed parameter struct: the parameter schema is generated from In and arguments are decoded automatically.
//
// In should be a struct. Fields without omitempty are required parameters; field descriptions are given with the `jsonschema:"description=..."` tag.
// A schema that cannot be generated from In is a programming error and panics.
func New[In any](name, description string, fn func(ctx *Context, in In) (*Result, error), opts ...Option) Tool {
	schema, err := chat.SchemaFor[In](chat.AllowOptional())
	if err != nil {
		panic(fmt.Sprintf("tool: cannot generate a schema for the parameters of tool %q: %v", name, err))
	}
	t := &typed{
		def: chat.ToolDef{Name: name, Description: description, InputSchema: schema},
		run: func(ctx *Context, args json.RawMessage) (*Result, error) {
			var in In
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, fmt.Errorf("decode tool arguments: %w", err)
			}
			return fn(ctx, in)
		},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

type typed struct {
	def        chat.ToolDef
	sequential bool
	timeout    time.Duration
	run        func(ctx *Context, args json.RawMessage) (*Result, error)
}

func (t *typed) Def() chat.ToolDef { return t.def }

func (t *typed) Execute(ctx *Context, args json.RawMessage) (*Result, error) { return t.run(ctx, args) }

func (t *typed) Sequential() bool { return t.sequential }

func (t *typed) Timeout() time.Duration { return t.timeout }
