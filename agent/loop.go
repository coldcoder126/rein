package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

// Run drives the Agent to the end: it calls the model, executes tools and returns their results to the model until the model gives a final reply.
//
// error is nil only when the run completes normally (Result.Status is StatusCompleted). A failed model call returns a *chat.Error,
// cancellation returns the ctx error, and reaching the turn limit returns ErrMaxTurnsExceeded; in these three cases Result is still valid
// and contains the messages produced so far. Result may be nil for invalid input, or when a toolset, middleware or event handler fails.
func (a *Agent) Run(ctx context.Context, in Input) (*Result, error) {
	if a.Model == nil && in.Model == nil {
		return nil, errors.New("agent: Model must not be nil")
	}
	if len(in.Messages) == 0 {
		return nil, errors.New("agent: Messages must not be empty")
	}
	if in.Messages[len(in.Messages)-1].Role == chat.RoleAssistant {
		return nil, errors.New("agent: Messages must not end with an assistant message")
	}
	// The nesting depth is put into ctx by the sub-agent tool and is 0 for the main run; this is the only place it enters the run info.
	depth, _ := ctx.Value(depthKey{}).(int)
	in.RunInfo.Depth = depth
	in.RunInfo.AgentName = a.Name
	if in.RunInfo.RootSessionID == "" {
		in.RunInfo.RootSessionID = in.RunInfo.SessionID
	}
	l := &loop{agent: *a, in: in, depth: depth, messages: slices.Clone(in.Messages), result: &Result{}, start: time.Now()}
	if in.Model != nil {
		l.agent.Model = in.Model
	}
	l.agent.applyDefaults()
	l.middlewares = slices.Concat(a.Middlewares, in.Middlewares)
	l.serializeEmit()
	ctx = context.WithValue(ctx, loopKey{}, l)
	return chainRun(l.middlewares, in.RunInfo, func(ctx context.Context) (*Result, error) {
		if err := l.resolve(ctx); err != nil {
			return nil, err
		}
		if err := l.run(ctx); err != nil {
			return l.result, err
		}
		return l.result, statusError(ctx, l.result)
	})(ctx)
}

// loopKey is the ctx key of the current run; the sub-agent tool uses it to find the main Agent that made the call.
type loopKey struct{}

// depthKey is the ctx key of the sub-agent nesting depth.
type depthKey struct{}

// statusError turns an end status other than normal completion into an error.
func statusError(ctx context.Context, result *Result) error {
	switch result.Status {
	case StatusFailed:
		return &chat.Error{Kind: result.Final.ErrorKind, Message: result.Final.ErrorMessage}
	case StatusAborted:
		if err := ctx.Err(); err != nil {
			return err
		}
		return chat.ErrAborted
	}
	return nil
}

func (a *Agent) applyDefaults() {
	if a.MaxTurns <= 0 {
		a.MaxTurns = defaultMaxTurns
	}
	if a.MaxParallelTools <= 0 {
		a.MaxParallelTools = defaultMaxParallelTools
	}
	if a.MaxToolResultBytes == 0 {
		a.MaxToolResultBytes = defaultMaxToolResultBytes
	}
}

// loop is the state of a run. agent is a copy of the definition, so the run never modifies the caller's Agent.
type loop struct {
	agent Agent
	in    Input
	// depth is the sub-agent nesting depth: 0 for the main run and 1 for a sub-agent. Sub-agents do not get the sub-agent tool.
	depth int
	// middlewares are the Agent's middlewares followed by this run's, the first being outermost.
	middlewares []Middleware

	instruction string
	tools       []tool.Tool
	messages    []chat.Message

	result *Result
	start  time.Time
}

// resolve settles the system prompt and tool list of the run at its start; they do not change afterwards.
func (l *loop) resolve(ctx context.Context) error {
	a := &l.agent
	l.instruction = a.Instruction
	if a.InstructionFunc != nil {
		text, err := a.InstructionFunc(ctx, l.in.RunInfo)
		if err != nil {
			return fmt.Errorf("agent: generate system prompt: %w", err)
		}
		l.instruction = text
	}
	l.tools = slices.Clone(a.Tools)
	for _, set := range a.Toolsets {
		tools, err := set.Tools(ctx, l.in.RunInfo)
		if err != nil {
			return fmt.Errorf("agent: resolve toolset: %w", err)
		}
		l.tools = append(l.tools, tools...)
		if instructor, ok := set.(tool.Instructor); ok {
			text, err := instructor.Instructions(ctx, l.in.RunInfo)
			if err != nil {
				return fmt.Errorf("agent: resolve toolset instructions: %w", err)
			}
			if strings.TrimSpace(text) != "" {
				l.instruction = strings.TrimSpace(strings.TrimRight(l.instruction, "\n") + "\n\n" + text)
			}
		}
	}
	if l.depth > 0 {
		l.instruction = strings.TrimSpace(strings.TrimRight(l.instruction, "\n") + "\n\n" + subAgentNotes)
	}
	// Tools that depend on background tasks (sub-agents, task management) appear only when the runtime provides a tool.TaskManager;
	// sub-agents cannot spawn sub-agents.
	hasManager := tool.TaskManagerFrom(ctx) != nil
	l.tools = slices.DeleteFunc(l.tools, func(t tool.Tool) bool {
		if nt, ok := t.(tool.NeedsTaskManager); ok && nt.NeedsTaskManager() && !hasManager {
			return true
		}
		_, ok := t.(*subAgentTool)
		return ok && l.depth > 0
	})
	seen := make(map[string]bool, len(l.tools))
	for _, t := range l.tools {
		name := t.Def().Name
		if seen[name] {
			return fmt.Errorf("agent: duplicate tool name %q", name)
		}
		seen[name] = true
	}
	return nil
}

// run is a two-level loop: the inner loop alternates between model calls and tool execution and injects steering messages between turns;
// when the inner loop has nothing left to do, the outer loop takes follow-up messages to decide whether to continue.
func (l *loop) run(ctx context.Context) error {
	if err := l.emit(ctx, Event{Type: EventAgentStart}); err != nil {
		return err
	}
	// The first turn always calls the model: the history ends with a user message or a tool result.
	more := true
	var pending []chat.Message
	for {
		for more || len(pending) > 0 {
			if l.result.Turns >= l.agent.MaxTurns {
				if err := l.finish(ctx, StatusMaxTurns); err != nil {
					return err
				}
				return ErrMaxTurnsExceeded
			}
			status, next, err := l.turn(ctx, pending)
			if err != nil {
				return err
			}
			if status != "" {
				return l.finish(ctx, status)
			}
			more = next
			if pending, err = poll(ctx, l.in.Steering); err != nil {
				return err
			}
		}
		var err error
		if pending, err = poll(ctx, l.in.FollowUp); err != nil {
			return err
		}
		if len(pending) == 0 {
			return l.finish(ctx, StatusCompleted)
		}
	}
}

// turn runs one turn through WrapTurn. A non-empty status means the run should end with it;
// more means the model must be called again: the turn has tool results to process, or a failed turn was recovered by a middleware.
func (l *loop) turn(ctx context.Context, pending []chat.Message) (status Status, more bool, err error) {
	info := TurnInfo{RunInfo: l.in.RunInfo, TurnIndex: l.result.Turns}
	called, failed := false, false
	result, err := chainTurn(l.middlewares, info, func(ctx context.Context) (*TurnResult, error) {
		if called {
			return nil, errors.New("agent: a turn can only run once")
		}
		called = true
		result, toolsRan, err := l.runTurn(ctx, info, pending)
		if result != nil {
			failed, more = result.Status != "", toolsRan
		}
		return result, err
	})(ctx)
	if err != nil {
		return "", false, err
	}
	if result == nil {
		return "", false, errors.New("agent: WrapTurn returned a nil result")
	}
	if result.Status != "" {
		return result.Status, false, nil
	}
	if len(result.Messages) == 0 {
		return "", false, errors.New("agent: TurnResult.Messages must not be empty when the run continues")
	}
	l.messages = slices.Clone(result.Messages)
	return "", more || failed, nil
}

// runTurn runs one turn: inject pending messages, call the model, execute tools. toolsRan reports that the turn has tool results for the model to process.
func (l *loop) runTurn(ctx context.Context, info TurnInfo, pending []chat.Message) (result *TurnResult, toolsRan bool, err error) {
	turnIndex := info.TurnIndex
	if err := l.emit(ctx, Event{Type: EventTurnStart, TurnIndex: turnIndex}); err != nil {
		return nil, false, err
	}
	for _, msg := range pending {
		if msg.CreatedAt.IsZero() {
			msg.CreatedAt = time.Now()
		}
		if err := l.addMessage(ctx, turnIndex, msg); err != nil {
			return nil, false, err
		}
	}

	assistant, offered, err := l.streamAssistant(ctx, info)
	if err != nil {
		return nil, false, err
	}
	l.messages = append(l.messages, assistant)
	l.result.Messages = append(l.result.Messages, assistant)
	l.result.Usage = l.result.Usage.Add(assistant.Usage)
	l.result.Turns++
	l.result.Final = &assistant

	result = &TurnResult{RunInfo: info.RunInfo, TurnIndex: turnIndex, Turn: Turn{Message: assistant}}
	switch assistant.StopReason {
	case chat.StopReasonError:
		result.Status = StatusFailed
	case chat.StopReasonAborted:
		result.Status = StatusAborted
	}
	terminate := false
	if result.Status == "" {
		var results []chat.Message
		if results, terminate, err = l.executeToolCalls(ctx, turnIndex, assistant, offered); err != nil {
			return nil, false, err
		}
		l.messages = append(l.messages, results...)
		l.result.Messages = append(l.result.Messages, results...)
		result.Turn.ToolResults = results
		// Canceled during tool execution: the results are complete, but the model should not continue.
		if len(results) > 0 && ctx.Err() != nil {
			result.Status = StatusAborted
		}
	}
	if err := l.emit(ctx, Event{Type: EventTurnEnd, TurnIndex: turnIndex, Turn: &result.Turn}); err != nil {
		return nil, false, err
	}
	result.Messages = slices.Clone(l.messages)
	return result, len(result.Turn.ToolResults) > 0 && !terminate, nil
}

// addMessage adds a completed message to the context and the result and emits the paired message events.
func (l *loop) addMessage(ctx context.Context, turnIndex int, msg chat.Message) error {
	l.messages = append(l.messages, msg)
	l.result.Messages = append(l.result.Messages, msg)
	return l.emitMessage(ctx, turnIndex, msg)
}

func (l *loop) emitMessage(ctx context.Context, turnIndex int, msg chat.Message) error {
	if err := l.emit(ctx, Event{Type: EventMessageStart, TurnIndex: turnIndex, Message: &msg}); err != nil {
		return err
	}
	return l.emit(ctx, Event{Type: EventMessageEnd, TurnIndex: turnIndex, Message: &msg})
}

// streamAssistant calls the model once through WrapModel and returns the assistant message and the names of the tools actually offered to the model.
func (l *loop) streamAssistant(ctx context.Context, info TurnInfo) (chat.Message, map[string]bool, error) {
	turnIndex := info.TurnIndex
	req := chat.Request{
		Options:      l.agent.Options,
		SystemPrompt: l.instruction,
		Messages:     slices.Clone(l.messages),
		Tools:        make([]chat.ToolDef, 0, len(l.tools)),
	}
	for _, t := range l.tools {
		req.Tools = append(req.Tools, t.Def())
	}
	req.Metadata = maps.Clone(req.Metadata)
	if req.Metadata == nil {
		req.Metadata = make(map[string]string, 1)
	}
	req.Metadata["turn_index"] = strconv.Itoa(turnIndex)

	// Middlewares may rewrite the request's tools; the last request actually sent to the model wins, and the original request's tools apply when the model was not called.
	offered := toolNames(req.Tools)
	stream := chainModel(l.middlewares, info, func(ctx context.Context, req chat.Request) iter.Seq[chat.Event] {
		offered = toolNames(req.Tools)
		return l.agent.Model.Stream(ctx, req)
	})

	var final *chat.Message
	for event := range stream(ctx, req) {
		var snapshot chat.Message
		if event.Message != nil {
			snapshot = snapshotOf(event.Message)
		}
		out := Event{Type: EventMessageUpdate, TurnIndex: turnIndex, Message: &snapshot}
		switch event.Type {
		case chat.EventStart:
			out.Type = EventMessageStart
		case chat.EventDone, chat.EventError:
			out.Type = EventMessageEnd
			final = &snapshot
		default:
			assistantEvent := event
			assistantEvent.Message = &snapshot
			out.AssistantEvent = &assistantEvent
		}
		if err := l.emit(ctx, out); err != nil {
			return chat.Message{}, nil, err
		}
	}
	if final == nil {
		return chat.Message{}, nil, errors.New("agent: model stream ended before a terminal event")
	}
	return *final, offered, nil
}

func toolNames(defs []chat.ToolDef) map[string]bool {
	names := make(map[string]bool, len(defs))
	for _, def := range defs {
		names[def.Name] = true
	}
	return names
}

// snapshotOf returns a snapshot of the streaming message's current state that later output does not change, so it is safe to keep.
// Media data and tool call arguments are never modified in place once generated, so the snapshot shares those bytes instead of
// copying them; otherwise replies with images or large arguments would be copied on every stream event. The snapshot is therefore read-only.
func snapshotOf(msg *chat.Message) chat.Message {
	out := *msg
	out.Content = slices.Clone(msg.Content)
	for i, block := range out.Content {
		if block.ToolCall != nil {
			call := *block.ToolCall
			out.Content[i].ToolCall = &call
		}
	}
	return out
}

// finish records the end status and emits agent_end.
func (l *loop) finish(ctx context.Context, status Status) error {
	l.result.Status = status
	l.result.Duration = time.Since(l.start)
	return l.emit(ctx, Event{Type: EventAgentEnd, RunResult: l.result})
}

func (l *loop) emit(ctx context.Context, event Event) error {
	if l.in.Emit == nil {
		return nil
	}
	return l.in.Emit(ctx, event)
}

func poll(ctx context.Context, fn func(context.Context) ([]chat.Message, error)) ([]chat.Message, error) {
	if fn == nil {
		return nil, nil
	}
	return fn(ctx)
}

// serializeEmit keeps the event handler from being called concurrently when tools run in parallel.
func (l *loop) serializeEmit() {
	if l.in.Emit == nil {
		return
	}
	sink := l.in.Emit
	var mu sync.Mutex
	l.in.Emit = func(ctx context.Context, event Event) error {
		mu.Lock()
		defer mu.Unlock()
		return sink(ctx, event)
	}
}
