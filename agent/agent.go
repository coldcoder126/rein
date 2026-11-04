// Package agent defines the Agent: a set of instructions, a model and tools, plus the decision loop that drives them.
//
// An Agent is a reusable definition that holds no session state: the same Agent can run for many sessions concurrently.
// Loading, persisting, queuing and compacting sessions is the job of the runner package.
package agent

import (
	"context"
	"errors"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

// ErrMaxTurnsExceeded means the loop had not ended when MaxTurns was reached. Result is still valid.
var ErrMaxTurnsExceeded = errors.New("agent: max turns exceeded")

const (
	defaultMaxTurns           = 20
	defaultMaxParallelTools   = 8
	defaultMaxToolResultBytes = 100_000
)

// Agent is the definition of an agent. Beyond the zero value, at least Model must be set, unless every run sets Input.Model.
//
// Extensions at the model call level (logging, usage, retries, fallbacks) wrap Model with chat.Chain;
// Middlewares cover the loop itself: the run, each turn, each turn's model request, and tool execution.
type Agent struct {
	// Name and Description identify the Agent. When it is given to SubAgentTool as a specialized sub-agent,
	// the model picks it by Name, and Description says what it is good at.
	Name        string
	Description string

	// Instruction is the system prompt. When InstructionFunc is set, it generates the prompt at the start of each run instead,
	// for prompts that vary per session. Instructions contributed by toolsets are appended after it.
	Instruction     string
	InstructionFunc func(ctx context.Context, info tool.RunInfo) (string, error)

	// Model is the default model; Input.Model overrides it for a single run.
	Model chat.Client
	// Options are passed unchanged to every model call; Metadata additionally carries turn_index.
	Options chat.Options

	// Tools are the fixed tools; Toolsets are resolved once at the start of each run and provide tools that vary per session.
	// Tool names must be unique across both.
	Tools    []tool.Tool
	Toolsets []tool.Toolset

	// MaxTurns limits the number of model calls in a run, 20 by default.
	MaxTurns int
	// SequentialTools runs each batch of tool calls sequentially; by default they run concurrently, and a batch runs sequentially when any of its tools declares tool.Sequential.
	SequentialTools bool
	// MaxParallelTools limits how many tools of a batch run concurrently, 8 by default.
	MaxParallelTools int
	// ToolTimeout is the default timeout of a single tool execution, 0 for none; a tool can declare its own with tool.Timeouter.
	// On timeout an error result is returned and the loop continues without waiting for the tool, so tools should honor ctx cancellation.
	ToolTimeout time.Duration
	// MaxToolResultBytes limits the total bytes of text in a single tool result, 100000 by default, negative for no limit.
	// Beyond it the start and end are kept with a note of how many bytes were omitted; images and Details are not affected.
	MaxToolResultBytes int

	// Middlewares are this Agent's own middlewares, the first being outermost; see Middleware. Sub-agents do not inherit them;
	// middlewares that apply to every Agent (approval, auditing, tracing) belong to the runtime and come from runner.Config.Middlewares.
	Middlewares []Middleware
}

// Input is the input of a run.
type Input struct {
	tool.RunInfo
	// Messages is the history so far and must end with a user message or a tool result.
	Messages []chat.Message
	// Emit receives the events of the run; returning an error stops the run.
	Emit EventSink
	// Steering is called after each turn; the messages it returns are injected before the next model call, for interjecting mid-run.
	// It never interrupts tools that are executing.
	Steering func(ctx context.Context) ([]chat.Message, error)
	// FollowUp is called when the loop would otherwise end; returning messages continues the run, for queued follow-up tasks.
	FollowUp func(ctx context.Context) ([]chat.Message, error)
	// Model overrides Agent.Model for this run when set. General sub-agents started by the run use it too;
	// specialized sub-agents keep their own Model. Agent.Options are passed unchanged, so leave model-specific
	// options such as Options.Model and MaxTokens unset when runs switch between models.
	Model chat.Client
	// Middlewares belong to this run only. They sit inside Agent.Middlewares and are not inherited by sub-agents.
	// Upper layers (such as the runner's history compaction) use them to hook into the run.
	Middlewares []Middleware
}

// TurnInfo describes a turn about to start. A turn is one iteration of the inner loop: inject pending messages, call the model once,
// and execute the tool calls it produced. TurnIndex is numbered consecutively from 0 within a run.
type TurnInfo struct {
	tool.RunInfo
	TurnIndex int
}

// TurnResult is the result of a turn. WrapTurn may change Status and Messages before passing it outward, and the loop continues from the outermost result.
// The turn's messages were already emitted as events during the turn (the runner persists them from those), so changing Turn does not affect them.
type TurnResult struct {
	tool.RunInfo
	TurnIndex int
	Turn      Turn
	// An empty Status continues the run; otherwise the run ends with that status: StatusFailed when the model failed, StatusAborted when canceled.
	// A middleware can set StatusCompleted to end the run without processing steering and follow-up;
	// clearing StatusFailed recovers from the failure, and the loop calls the model again with Messages. It should then also replace Messages
	// (by compacting history, for example), or the next turn fails the same way. The recovered turn also counts toward MaxTurns.
	Status Status
	// Messages is the full history including this turn; when the run continues it becomes the history of later turns, so replacing it compacts history.
	// Result.Messages is still every message produced by the run and is not affected by the replacement.
	Messages []chat.Message
}

// ToolInvocation is one tool call, passed to WrapToolFunc.
type ToolInvocation struct {
	tool.RunInfo
	TurnIndex        int
	AssistantMessage chat.Message
	Call             chat.ToolCall
	Tool             tool.Tool
	// Args are the arguments about to be passed to the tool, already validated against the schema; they are validated again if a middleware changes them.
	Args []byte
}

// ToolNext continues the tool call. Errors and panics from the tool have already been turned into IsError results.
type ToolNext func(ctx context.Context, inv *ToolInvocation) *tool.Result

// WrapToolFunc wraps one tool execution: it can change arguments, rewrite the result, time it, or ask for human approval,
// and it can intercept the call by returning a result without calling next. It is called concurrently when tools run in parallel.
// A returned error does not stop the run; it is given to the model as an error result.
type WrapToolFunc func(ctx context.Context, inv *ToolInvocation, next ToolNext) (*tool.Result, error)

// Status is how a run ended.
type Status string

const (
	// StatusCompleted means the model gave a final reply, or the run ended normally through WrapTurn or a tool's Terminate.
	StatusCompleted Status = "completed"
	// StatusMaxTurns means MaxTurns was reached; ErrMaxTurnsExceeded is returned as well.
	StatusMaxTurns Status = "max_turns"
	// StatusFailed means a model call failed; Run also returns the corresponding *chat.Error.
	StatusFailed Status = "failed"
	// StatusAborted means ctx was canceled; Run also returns the ctx error.
	StatusAborted Status = "aborted"
)

// Result is the result of a run.
type Result struct {
	Status Status
	// Messages are all messages added by the run, in the order they entered history.
	Messages []chat.Message
	// Final is the last assistant message.
	Final *chat.Message
	// Usage is the total tokens consumed by model calls and inside tools.
	Usage    chat.Usage
	Turns    int
	Duration time.Duration
}
