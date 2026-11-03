// Package tool defines the tools an Agent can call: the tool interface, the execution context, results, and toolsets that provide tools in bulk.
//
// Writing a tool only needs this package; the Agent definition and loop live in the agent package, and toolsets such as skills in their own packages.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/coldcoder126/rein/chat"
)

// Tool is a tool the model can call.
//
// An error returned by Execute does not stop the run; it is given to the model as an error result so it can correct itself.
// Arguments are validated against Def().InputSchema before the call.
type Tool interface {
	Def() chat.ToolDef
	Execute(ctx *Context, args json.RawMessage) (*Result, error)
}

// Sequential is an optional interface for tools: returning true means the whole batch of calls containing it must run sequentially,
// for tools with side effects or that cannot run concurrently.
type Sequential interface {
	Sequential() bool
}

// Timeouter is an optional interface for tools that declare their own execution timeout, which takes precedence over the Agent's default.
type Timeouter interface {
	Timeout() time.Duration
}

// RunInfo identifies a run. When an Agent is used directly without a session, the session fields may be empty.
type RunInfo struct {
	// SessionID is the session this run writes to. Every sub-agent task has a session of its own,
	// so inside a sub-agent it is not the user's session; use RootSessionID for that.
	SessionID string
	RunID     string
	// ParentSessionID is the session that started this sub-agent task, or empty when it is not one.
	ParentSessionID string
	// RootSessionID is the session where everything started: it equals SessionID in the main run and is passed down through parent sessions to sub-agent tasks.
	// Tools that look up data by the user's conversation should use it. When empty, the Agent runtime sets it to SessionID.
	RootSessionID string
	// Depth is the sub-agent nesting depth: 0 for the main Agent and 1 for the sub-agents it spawns.
	// Middlewares such as approval and auditing can use it to tell who made a call. Set by the Agent runtime.
	Depth int
	// AgentName is the Name of the running Agent, empty for a general sub-agent. Set by the Agent runtime.
	AgentName string
}

// Context is what a tool receives when it executes. It is a context.Context, so cancellation and timeouts work as usual,
// and it also carries the session, run and call of this invocation.
type Context struct {
	context.Context
	RunInfo
	// Call is the tool call issued by the model.
	Call chat.ToolCall

	update func(Update) error
}

// NewContext creates a tool execution context. Progress reports are ignored when update is nil. Tests of tools can build a context with it directly.
func NewContext(ctx context.Context, info RunInfo, call chat.ToolCall, update func(Update) error) *Context {
	return &Context{Context: ctx, RunInfo: info, Call: call, update: update}
}

// Update reports execution progress, mainly for UIs and logs. It returns an error once the execution timed out and was abandoned.
func (c *Context) Update(u Update) error {
	if c.update == nil {
		return nil
	}
	return c.update(u)
}

// Update is a partial result during tool execution.
type Update struct {
	Content []chat.ContentBlock
	Details any
}

// Result is the final result of a tool execution.
type Result struct {
	// Content enters the model context of the next turn.
	Content []chat.ContentBlock
	// Details is structured data for the UI, logs and business, never sent to the model.
	Details any
	// IsError marks an error result.
	IsError bool
	// Terminate asks to end the run after this turn; it takes effect only when every result of the batch asks for it.
	Terminate bool
	// Usage is the tokens consumed inside the tool (for example when the tool calls a model itself), counted in the run's total usage.
	Usage chat.Usage
}

// Text returns a result with a single text block.
func Text(text string) *Result {
	return &Result{Content: []chat.ContentBlock{chat.Text(text)}}
}

// Error returns an error result in text form.
func Error(text string) *Result {
	return &Result{Content: []chat.ContentBlock{chat.Text(text)}, IsError: true}
}

// Toolset is a source that provides tools in bulk, such as a set of skills or an MCP server.
//
// Tools is called once at the start of each run and may return different tools per session; the tool list stays the same within a run,
// so the prompt cache is not broken.
type Toolset interface {
	Tools(ctx context.Context, info RunInfo) ([]Tool, error)
}

// Instructor is an optional interface for toolsets: the returned text is appended to the system prompt
// to tell the model how to use these tools (such as the catalog of available skills). An empty string appends nothing.
type Instructor interface {
	Instructions(ctx context.Context, info RunInfo) (string, error)
}

// ErrAbandoned is the error a tool gets when it reports progress after its execution timed out and was abandoned.
var ErrAbandoned = errors.New("tool: execution timed out, progress is no longer reported")
