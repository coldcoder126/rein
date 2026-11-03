package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

const (
	truncatedCallText = "The model output was truncated, so the tool call arguments are incomplete and the call was not executed. Retry with shorter arguments."
	abortedCallText   = "The operation was canceled; the tool was not executed."
)

// executeToolCalls executes every tool call in the assistant message and guarantees each call one result.
// The tool results are returned in the original call order; terminate reports that every result asked to end the loop.
// A returned error only comes from the event handler, and by then all in-flight tools have finished.
// offered are the names of the tools actually offered to the model in this turn's request; calls outside it are not executed.
func (l *loop) executeToolCalls(ctx context.Context, turnIndex int, assistant chat.Message, offered map[string]bool) ([]chat.Message, bool, error) {
	calls := assistant.ToolCalls()
	if len(calls) == 0 {
		return nil, false, nil
	}
	results := make([]*tool.Result, len(calls))
	var runnable []*ToolInvocation
	var indexes []int
	sequential := l.agent.SequentialTools

	for i, call := range calls {
		if err := l.emit(ctx, Event{Type: EventToolExecutionStart, TurnIndex: turnIndex, ToolCall: &call}); err != nil {
			return nil, false, err
		}
		inv, failure := l.prepare(turnIndex, assistant, call, offered)
		if failure != nil {
			results[i] = failure
			if err := l.emitToolEnd(ctx, turnIndex, call, failure); err != nil {
				return nil, false, err
			}
			continue
		}
		if seq, ok := inv.Tool.(tool.Sequential); ok && seq.Sequential() {
			sequential = true
		}
		runnable = append(runnable, inv)
		indexes = append(indexes, i)
	}

	var emitErr error
	record := func(n int, result *tool.Result) {
		results[indexes[n]] = result
		if err := l.emitToolEnd(ctx, turnIndex, runnable[n].Call, result); err != nil && emitErr == nil {
			emitErr = err
		}
	}
	if sequential {
		for n, inv := range runnable {
			record(n, l.invoke(ctx, inv))
		}
	} else {
		type done struct {
			n      int
			result *tool.Result
		}
		ch := make(chan done, len(runnable))
		slots := make(chan struct{}, l.agent.MaxParallelTools)
		for n, inv := range runnable {
			go func() {
				slots <- struct{}{}
				defer func() { <-slots }()
				ch <- done{n, l.invoke(ctx, inv)}
			}()
		}
		// Wait for every tool to finish even if the event handler failed, so no running goroutines are left behind.
		for range runnable {
			d := <-ch
			record(d.n, d.result)
		}
	}
	if emitErr != nil {
		return nil, false, emitErr
	}

	messages := make([]chat.Message, 0, len(calls))
	terminate := true
	for i, call := range calls {
		msg := chat.Message{
			Role:       chat.RoleTool,
			Content:    results[i].Content,
			ToolCallID: call.ID,
			ToolName:   call.Name,
			Details:    results[i].Details,
			IsError:    results[i].IsError,
			CreatedAt:  time.Now(),
		}
		if err := l.emitMessage(ctx, turnIndex, msg); err != nil {
			return nil, false, err
		}
		messages = append(messages, msg)
		terminate = terminate && results[i].Terminate
		l.result.Usage = l.result.Usage.Add(results[i].Usage)
	}
	return messages, terminate, nil
}

// prepare looks up the tool and validates the arguments, returning an error result when the call cannot run.
func (l *loop) prepare(turnIndex int, assistant chat.Message, call chat.ToolCall, offered map[string]bool) (*ToolInvocation, *tool.Result) {
	if assistant.StopReason == chat.StopReasonLength {
		return nil, errorResult(truncatedCallText)
	}
	var found tool.Tool
	for _, t := range l.tools {
		if t.Def().Name == call.Name {
			found = t
			break
		}
	}
	if found == nil {
		return nil, errorResult(fmt.Sprintf("tool %q not found", call.Name))
	}
	if !offered[call.Name] {
		return nil, errorResult(fmt.Sprintf("tool %q is not available in this turn and cannot be called", call.Name))
	}
	args := []byte(call.Arguments)
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	if err := validateArgs(found, args); err != nil {
		return nil, errorResult(err.Error())
	}
	return &ToolInvocation{RunInfo: l.in.RunInfo, TurnIndex: turnIndex, AssistantMessage: assistant, Call: call, Tool: found, Args: args}, nil
}

func validateArgs(t tool.Tool, args []byte) error {
	if !json.Valid(args) {
		return fmt.Errorf("tool arguments are not valid JSON")
	}
	schema := t.Def().InputSchema
	if len(schema) == 0 {
		return nil
	}
	if err := chat.ValidateJSON(schema, args); err != nil {
		return fmt.Errorf("tool arguments do not match the schema: %w", err)
	}
	return nil
}

// invoke executes one tool call through WrapTool and caps the size of the result.
func (l *loop) invoke(ctx context.Context, inv *ToolInvocation) *tool.Result {
	return limitResult(l.wrapAndExecute(ctx, inv), l.agent.MaxToolResultBytes)
}

// wrapAndExecute turns errors and panics from middlewares and tools into error results.
func (l *loop) wrapAndExecute(ctx context.Context, inv *ToolInvocation) (result *tool.Result) {
	defer func() {
		if r := recover(); r != nil {
			result = errorResult(fmt.Sprintf("tool %q panicked: %v", inv.Call.Name, r))
		}
	}()
	if ctx.Err() != nil {
		return errorResult(abortedCallText)
	}
	original := bytes.Clone(inv.Args)
	next := func(ctx context.Context, inv *ToolInvocation) *tool.Result {
		if !bytes.Equal(inv.Args, original) {
			if err := validateArgs(inv.Tool, inv.Args); err != nil {
				return errorResult("arguments changed by a middleware are invalid: " + err.Error())
			}
		}
		return l.execute(ctx, inv)
	}
	return chainTool(l.middlewares, next)(ctx, inv)
}

// execute runs the tool with its timeout. After a timeout it stops waiting: the tool's goroutine runs until it ends on its own,
// and progress it reports afterwards is dropped. Cancellation of the parent ctx is not a timeout; the tool is still awaited.
func (l *loop) execute(ctx context.Context, inv *ToolInvocation) *tool.Result {
	timeout := l.agent.ToolTimeout
	if t, ok := inv.Tool.(tool.Timeouter); ok && t.Timeout() > 0 {
		timeout = t.Timeout()
	}
	var abandoned atomic.Bool
	if timeout <= 0 {
		return l.call(ctx, inv, &abandoned)
	}
	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	timedOut := errorResult(fmt.Sprintf("tool %q timed out (%s)", inv.Call.Name, timeout))
	ch := make(chan *tool.Result, 1)
	go func() { ch <- l.call(toolCtx, inv, &abandoned) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-ch:
		// A tool that returned on its own because of the timed-out ctx gets the timeout result too.
		if ctx.Err() == nil && errors.Is(toolCtx.Err(), context.DeadlineExceeded) {
			return timedOut
		}
		return result
	case <-timer.C:
		abandoned.Store(true)
		return timedOut
	}
}

func (l *loop) call(ctx context.Context, inv *ToolInvocation, abandoned *atomic.Bool) (result *tool.Result) {
	defer func() {
		if r := recover(); r != nil {
			result = errorResult(fmt.Sprintf("tool %q panicked: %v", inv.Call.Name, r))
		}
	}()
	update := func(update tool.Update) error {
		if abandoned.Load() {
			return tool.ErrAbandoned
		}
		return l.emit(ctx, Event{Type: EventToolExecutionUpdate, TurnIndex: inv.TurnIndex, ToolCall: &inv.Call, Update: &update})
	}
	result, err := inv.Tool.Execute(tool.NewContext(ctx, inv.RunInfo, inv.Call, update), inv.Args)
	if err != nil {
		return errorResult(err.Error())
	}
	if result == nil {
		return &tool.Result{Content: []chat.ContentBlock{chat.Text("")}}
	}
	return result
}

// limitResult caps the total text in a result at limit bytes: beyond it each text block keeps its start and end proportionally.
func limitResult(result *tool.Result, limit int) *tool.Result {
	if limit < 0 {
		return result
	}
	total := 0
	for _, block := range result.Content {
		if block.Type == chat.ContentTypeText {
			total += len(block.Text)
		}
	}
	if total <= limit {
		return result
	}
	out := *result
	out.Content = make([]chat.ContentBlock, len(result.Content))
	for i, block := range result.Content {
		if block.Type == chat.ContentTypeText {
			block.Text = truncateMiddle(block.Text, limit*len(block.Text)/total)
		}
		out.Content[i] = block
	}
	return &out
}

// truncateMiddle keeps about keep bytes of text from the start and end together, cutting on character boundaries.
func truncateMiddle(text string, keep int) string {
	if len(text) <= keep {
		return text
	}
	head, tail := keep/2, len(text)-(keep-keep/2)
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}
	return fmt.Sprintf("%s\n\n[... tool result too long, %d bytes omitted from the middle ...]\n\n%s", text[:head], tail-head, text[tail:])
}

func (l *loop) emitToolEnd(ctx context.Context, turnIndex int, call chat.ToolCall, result *tool.Result) error {
	return l.emit(ctx, Event{Type: EventToolExecutionEnd, TurnIndex: turnIndex, ToolCall: &call, Result: result})
}

func errorResult(text string) *tool.Result {
	return &tool.Result{Content: []chat.ContentBlock{chat.Text(text)}, IsError: true}
}
