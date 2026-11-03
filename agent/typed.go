package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/coldcoder126/rein/chat"
)

// Text returns an input with a single user text message.
func Text(text string) Input {
	return Input{Messages: []chat.Message{chat.NewUserTextMessage(text)}}
}

// Parts returns an input with a single user message made of several content blocks, such as some text and a few images.
func Parts(blocks ...chat.ContentBlock) Input {
	return Input{Messages: []chat.Message{{Role: chat.RoleUser, Content: blocks, CreatedAt: time.Now()}}}
}

// RunJSON runs the Agent and decodes the final reply into T, for cases that need a structured result, such as a step in a workflow.
//
// When a.Options.Output is nil the schema is generated from T (T should be a struct). Tools, middlewares and events work as usual,
// and the format only constrains the final reply. When the reply is not valid JSON or does not match the schema, the validation error
// is given to the model and the Agent runs once more; this repair emits another pair of agent_start / agent_end events.
//
// The format constraint is sent with every model call, and capable models still call tools in intermediate turns. Some weaker models
// (claude-haiku-4-5 in testing) may skip the tools under the constraint and make up a result; switch to a stronger model when that happens.
//
// Errors mean the same as for Run; in addition, output still invalid after the repair returns a *chat.Error of Kind chat.KindInvalidOutput.
// Result is still valid when error is non-nil, with the messages and usage of both runs.
func RunJSON[T any](ctx context.Context, a *Agent, in Input) (T, *Result, error) {
	var out T
	typed := *a
	if typed.Options.Output == nil {
		schema, err := chat.SchemaFor[T]()
		if err != nil {
			return out, nil, &chat.Error{Kind: chat.KindInvalidRequest, Err: err}
		}
		typed.Options.Output = &chat.OutputFormat{Schema: schema}
	}

	result, err := typed.Run(ctx, in)
	if err != nil {
		return out, result, err
	}
	raw, err := chat.ParseOutput(result.Final, typed.Options.Output)
	if repairable(result, err) {
		repair := chat.NewUserTextMessage(fmt.Sprintf(
			"Your previous reply did not follow the required output format: %v. Give your final reply again, outputting only JSON that matches the schema and nothing else.", err))
		in.Messages = slices.Concat(in.Messages, result.Messages, []chat.Message{repair})
		second, runErr := typed.Run(ctx, in)
		if second == nil {
			return out, result, runErr
		}
		result = mergeResults(result, repair, second)
		if runErr != nil {
			return out, result, runErr
		}
		raw, err = chat.ParseOutput(result.Final, typed.Options.Output)
	}
	if err != nil {
		return out, result, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, result, &chat.Error{Kind: chat.KindInvalidOutput, Message: "output cannot be decoded into the target type", Err: err}
	}
	return out, result, nil
}

// repairable reports whether an output error is worth a retry by the model: only invalid content is retried,
// since truncated output would be truncated again.
func repairable(result *Result, err error) bool {
	var ce *chat.Error
	return errors.As(err, &ce) && ce.Kind == chat.KindInvalidOutput &&
		result.Final != nil && result.Final.StopReason != chat.StopReasonLength
}

// mergeResults merges the repair run into the result of the first run; repair is the message injected between the two runs.
func mergeResults(first *Result, repair chat.Message, second *Result) *Result {
	return &Result{
		Status:   second.Status,
		Messages: slices.Concat(first.Messages, []chat.Message{repair}, second.Messages),
		Final:    second.Final,
		Usage:    first.Usage.Add(second.Usage),
		Turns:    first.Turns + second.Turns,
		Duration: first.Duration + second.Duration,
	}
}
