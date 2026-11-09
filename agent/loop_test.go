package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
	"github.com/coldcoder126/rein/tool"
)

type stubTool struct {
	name       string
	delay      time.Duration
	sequential bool
	schema     string
	run        func(ctx *tool.Context, args json.RawMessage) (*tool.Result, error)
}

func (t stubTool) Def() chat.ToolDef {
	schema := t.schema
	if schema == "" {
		schema = `{"type":"object"}`
	}
	return chat.ToolDef{Name: t.name, InputSchema: json.RawMessage(schema)}
}

func (t stubTool) Execute(ctx *tool.Context, args json.RawMessage) (*tool.Result, error) {
	if t.delay > 0 {
		time.Sleep(t.delay)
	}
	if t.run != nil {
		return t.run(ctx, args)
	}
	return tool.Text(t.name + " result"), nil
}

func (t stubTool) Sequential() bool { return t.sequential }

func user(text string) []chat.Message { return []chat.Message{chat.NewUserTextMessage(text)} }

func call(id, name, args string) chat.ToolCall {
	return chat.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// toolTexts returns the text of each tool result in the result, in order.
func toolTexts(result *Result) []string {
	var out []string
	for _, msg := range result.Messages {
		if msg.Role == chat.RoleTool {
			out = append(out, msg.TextContent())
		}
	}
	return out
}

func roles(msgs []chat.Message) string {
	var out []string
	for _, msg := range msgs {
		out = append(out, string(msg.Role))
	}
	return strings.Join(out, ",")
}

func TestRunParallelToolsEndByCompletionMessageBySourceOrder(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(call("slow_id", "slow", ""), call("fast_id", "fast", "")), chattest.Say("完成"))
	var endOrder, messageOrder []string
	result, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{stubTool{name: "slow", delay: 30 * time.Millisecond}, stubTool{name: "fast"}},
	}).Run(context.Background(), Input{
		Messages: user("执行工具"),
		Emit: func(_ context.Context, event Event) error {
			if event.Type == EventToolExecutionEnd {
				endOrder = append(endOrder, event.ToolCall.Name)
			}
			if event.Type == EventMessageEnd && event.Message.Role == chat.RoleTool {
				messageOrder = append(messageOrder, event.Message.ToolName)
			}
			return nil
		},
	})
	if err != nil || result.Status != StatusCompleted || result.Turns != 2 {
		t.Fatalf("should complete two turns normally: %+v %v", result, err)
	}
	if got := strings.Join(endOrder, ","); got != "fast,slow" {
		t.Fatalf("tool end events should follow completion order: %s", got)
	}
	if got := strings.Join(messageOrder, ","); got != "slow,fast" {
		t.Fatalf("tool result messages should follow call order: %s", got)
	}
}

func TestRunSequentialToolForcesWholeBatchSequential(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(call("1", "slow", ""), call("2", "fast", "")), chattest.Say("完成"))
	var endOrder []string
	_, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{stubTool{name: "slow", delay: 20 * time.Millisecond, sequential: true}, stubTool{name: "fast"}},
	}).Run(context.Background(), Input{
		Messages: user("执行"),
		Emit: func(_ context.Context, event Event) error {
			if event.Type == EventToolExecutionEnd {
				endOrder = append(endOrder, event.ToolCall.Name)
			}
			return nil
		},
	})
	if err != nil || strings.Join(endOrder, ",") != "slow,fast" {
		t.Fatalf("the whole batch should run sequentially when any tool requires it: %v %v", endOrder, err)
	}
}

func TestRunPassesOptionsAndTurnIndex(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(call("call_1", "lookup", "")), chattest.Say("完成"))
	_, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{stubTool{name: "lookup"}},
		Options: chat.Options{
			Model: "m1", MaxTokens: 77, Temperature: chat.Float(0), Reasoning: chat.ReasoningHigh,
			Metadata: map[string]string{"run_id": "run-1", "turn_index": "caller"},
		},
	}).Run(context.Background(), Input{
		Messages: user("查询"),
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := client.Requests()
	if len(requests) != 2 {
		t.Fatalf("expected two model requests: %d", len(requests))
	}
	for i, req := range requests {
		if req.Model != "m1" || req.MaxTokens != 77 || req.Temperature == nil || *req.Temperature != 0 || req.Reasoning != chat.ReasoningHigh {
			t.Fatalf("model options should be passed through unchanged: %+v", req.Options)
		}
		if req.Metadata["run_id"] != "run-1" || req.Metadata["turn_index"] != string(rune('0'+i)) {
			t.Fatalf("wrong metadata: %+v", req.Metadata)
		}
	}
}

func TestInputModelOverridesAgentModel(t *testing.T) {
	client := chattest.NewClient(chattest.Say("完成"))
	if _, err := (&Agent{}).Run(context.Background(), Input{Messages: user("查询"), Model: client}); err != nil || len(client.Requests()) != 1 {
		t.Fatalf("the run should use Input.Model: %v requests=%d", err, len(client.Requests()))
	}
	if _, err := (&Agent{}).Run(context.Background(), Input{Messages: user("查询")}); err == nil {
		t.Fatal("a run without any model should fail")
	}
}

func TestRunEmitsAssistantStreamEvents(t *testing.T) {
	client := chattest.NewClient(func(_ context.Context, b *chat.Builder) {
		b.ThinkingDelta("想")
		b.TextDelta("你")
		b.TextDelta("好")
		b.Done("")
	})
	var types []chat.EventType
	var lifecycle []string
	var lastSnapshot string
	result, err := (&Agent{
		Model: client,
	}).Run(context.Background(), Input{
		Messages: user("hi"),
		Emit: func(_ context.Context, event Event) error {
			if event.Type == EventMessageUpdate {
				types = append(types, event.AssistantEvent.Type)
				lastSnapshot = event.Message.TextContent()
			} else {
				lifecycle = append(lifecycle, string(event.Type))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []chat.EventType{
		chat.EventThinkingStart, chat.EventThinkingDelta, chat.EventThinkingEnd,
		chat.EventTextStart, chat.EventTextDelta, chat.EventTextDelta, chat.EventTextEnd,
	}
	if len(types) != len(want) {
		t.Fatalf("wrong message_update event sequence: %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("wrong message_update event sequence: %v", types)
		}
	}
	if got := strings.Join(lifecycle, ","); got != "agent_start,turn_start,message_start,message_end,turn_end,agent_end" {
		t.Fatalf("wrong lifecycle event order: %s", got)
	}
	if lastSnapshot != "你好" || result.Final.TextContent() != "你好" {
		t.Fatalf("wrong snapshot content: snapshot=%q final=%q", lastSnapshot, result.Final.TextContent())
	}
}

func TestRunKeepsBlockOrderAcrossTurns(t *testing.T) {
	client := chattest.NewClient(
		func(_ context.Context, b *chat.Builder) {
			b.ThinkingDelta("先想")
			b.ThinkingSignature("sig")
			b.TextDelta("我来查")
			b.ToolCall("call_1", "lookup", nil)
			b.Done("")
		},
		chattest.Say("完成"),
	)
	_, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{stubTool{name: "lookup"}},
	}).Run(context.Background(), Input{
		Messages: user("查"),
	})
	if err != nil {
		t.Fatal(err)
	}
	second := client.Requests()[1].Messages
	if roles(second) != "user,assistant,tool" {
		t.Fatalf("wrong context structure in the second turn: %s", roles(second))
	}
	for i, want := range []chat.ContentType{chat.ContentTypeThinking, chat.ContentTypeText, chat.ContentTypeToolCall} {
		if second[1].Content[i].Type != want {
			t.Fatalf("assistant content block order changed: %+v", second[1].Content)
		}
	}
}

func TestRunModelFailureEndsRunWithFailedMessage(t *testing.T) {
	client := chattest.NewClient(func(_ context.Context, b *chat.Builder) {
		b.TextDelta("半截")
		b.Fail(&chat.Error{Kind: chat.KindServer, Message: "上游过载"})
	})
	var sawEnd, sawAgentEnd bool
	result, err := (&Agent{
		Model: client,
	}).Run(context.Background(), Input{
		Messages: user("hi"),
		Emit: func(_ context.Context, event Event) error {
			sawEnd = sawEnd || (event.Type == EventMessageEnd && event.Message.StopReason == chat.StopReasonError)
			sawAgentEnd = sawAgentEnd || event.Type == EventAgentEnd
			return nil
		},
	})
	var ce *chat.Error
	if !errors.As(err, &ce) || ce.Kind != chat.KindServer {
		t.Fatalf("a model failure should return a classified *chat.Error with a valid result: %v", err)
	}
	if !sawEnd || !sawAgentEnd || result.Status != StatusFailed {
		t.Fatalf("a failure should also emit message_end and agent_end: end=%v agentEnd=%v status=%s", sawEnd, sawAgentEnd, result.Status)
	}
	final := result.Final
	if final == nil || final.ErrorKind != chat.KindServer || final.TextContent() != "半截" || len(result.Messages) != 1 {
		t.Fatalf("the final message should keep the failure and the content generated so far: %+v", final)
	}
}

func TestRunEmitErrorStopsRun(t *testing.T) {
	boom := errors.New("emit 失败")
	_, err := (&Agent{
		Model: chattest.NewClient(chattest.Say("你好")),
	}).Run(context.Background(), Input{
		Messages: user("hi"),
		Emit: func(_ context.Context, event Event) error {
			if event.Type == EventMessageUpdate {
				return boom
			}
			return nil
		},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("an Emit error should stop the run: %v", err)
	}
}

func TestRunMaxTurnsEmitsAgentEnd(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(call("1", "t", "")), chattest.CallTools(call("2", "t", "")), chattest.Say("不应到达"))
	var ended bool
	result, err := (&Agent{
		Model:    client,
		Tools:    []tool.Tool{stubTool{name: "t"}},
		MaxTurns: 2,
	}).Run(context.Background(), Input{
		Messages: user("循环"),
		Emit: func(_ context.Context, event Event) error {
			ended = ended || event.Type == EventAgentEnd
			return nil
		},
	})
	if !errors.Is(err, ErrMaxTurnsExceeded) || result.Status != StatusMaxTurns || result.Turns != 2 || !ended {
		t.Fatalf("should end with max_turns and emit agent_end: %+v %v ended=%v", result, err, ended)
	}
	if len(toolTexts(result)) != 2 {
		t.Fatalf("results of executed tools should be kept: %+v", result.Messages)
	}
}

func TestToolFailuresBecomeErrorResults(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(
		call("1", "missing", `{}`),
		call("2", "strict", `{"n":"三"}`),
		call("3", "strict", `{"n":3}`),
		call("4", "panics", `{}`),
		call("5", "fails", `{}`),
	), chattest.Say("完成"))
	result, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{
			stubTool{name: "strict", schema: `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`},
			stubTool{name: "panics", run: func(*tool.Context, json.RawMessage) (*tool.Result, error) { panic("炸了") }},
			stubTool{name: "fails", run: func(*tool.Context, json.RawMessage) (*tool.Result, error) {
				return nil, errors.New("下游超时")
			}},
		},
	}).Run(context.Background(), Input{
		Messages: user("执行"),
	})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("tool failures should not stop the run: %+v %v", result, err)
	}
	texts := toolTexts(result)
	wants := []string{"not found", "do not match the schema", "strict result", "panicked: 炸了", "下游超时"}
	for i, want := range wants {
		if !strings.Contains(texts[i], want) {
			t.Fatalf("result %d should contain %q: %q", i, want, texts[i])
		}
	}
	isError := []bool{true, true, false, true, true}
	n := 0
	for _, msg := range result.Messages {
		if msg.Role == chat.RoleTool {
			if msg.IsError != isError[n] {
				t.Fatalf("result %d has the wrong IsError: %+v", n, msg)
			}
			n++
		}
	}
}

func TestTruncatedToolCallsAreNotExecuted(t *testing.T) {
	var executed atomic.Int32
	client := chattest.NewClient(func(_ context.Context, b *chat.Builder) {
		b.ToolCall("1", "write", json.RawMessage(`{}`))
		b.Done(chat.StopReasonLength)
	}, chattest.Say("完成"))
	result, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{
			stubTool{name: "write", run: func(*tool.Context, json.RawMessage) (*tool.Result, error) {
				executed.Add(1)
				return tool.Text("ok"), nil
			}},
		},
	}).Run(context.Background(), Input{
		Messages: user("写文件"),
	})
	if err != nil || executed.Load() != 0 {
		t.Fatalf("truncated calls should not be executed: executed=%d err=%v", executed.Load(), err)
	}
	if texts := toolTexts(result); len(texts) != 1 || !strings.Contains(texts[0], "truncated") || result.Turns != 2 {
		t.Fatalf("should add an error result and let the model retry: %v turns=%d", texts, result.Turns)
	}
}

func TestCancelDuringToolsPairsEveryCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := chattest.NewClient(chattest.CallTools(call("1", "first", ""), call("2", "second", "")), chattest.Say("不应到达"))
	var secondRan bool
	result, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{
			stubTool{name: "first", sequential: true, run: func(*tool.Context, json.RawMessage) (*tool.Result, error) {
				cancel()
				return tool.Text("first done"), nil
			}},
			stubTool{name: "second", run: func(*tool.Context, json.RawMessage) (*tool.Result, error) {
				secondRan = true
				return tool.Text("second done"), nil
			}},
		},
	}).Run(ctx, Input{
		Messages: user("执行"),
	})
	if !errors.Is(err, context.Canceled) || result.Status != StatusAborted || secondRan || len(client.Requests()) != 1 {
		t.Fatalf("should stop after canceling without calling the model again: %+v err=%v secondRan=%v", result, err, secondRan)
	}
	texts := toolTexts(result)
	if len(texts) != 2 || texts[0] != "first done" || !strings.Contains(texts[1], "canceled") {
		t.Fatalf("every call should have a result: %v", texts)
	}
}

func TestEmitErrorWaitsForRunningTools(t *testing.T) {
	boom := errors.New("emit 失败")
	var finished atomic.Int32
	slow := func(*tool.Context, json.RawMessage) (*tool.Result, error) {
		time.Sleep(30 * time.Millisecond)
		finished.Add(1)
		return tool.Text("ok"), nil
	}
	_, err := (&Agent{
		Model: chattest.NewClient(chattest.CallTools(call("1", "fast", ""), call("2", "slow", ""))),
		Tools: []tool.Tool{
			stubTool{name: "fast"}, stubTool{name: "slow", run: slow},
		},
	}).Run(context.Background(), Input{
		Messages: user("执行"),
		Emit: func(_ context.Context, event Event) error {
			if event.Type == EventToolExecutionEnd {
				return boom
			}
			return nil
		},
	})
	if !errors.Is(err, boom) || finished.Load() != 1 {
		t.Fatalf("should wait for in-flight tools before returning: err=%v finished=%d", err, finished.Load())
	}
}

func TestSteeringAndFollowUp(t *testing.T) {
	client := chattest.NewClient(
		chattest.CallTools(call("1", "t", "")), // turn 0: call the tool
		chattest.Say("已按新要求处理"),                // turn 1: sees the steering message
		chattest.Say("后续任务完成"),                 // turn 2: follow-up
	)
	steering := [][]chat.Message{user("改一下方向")}
	followUp := [][]chat.Message{user("再做一件事")}
	take := func(queue *[][]chat.Message) func(context.Context) ([]chat.Message, error) {
		return func(context.Context) ([]chat.Message, error) {
			if len(*queue) == 0 {
				return nil, nil
			}
			next := (*queue)[0]
			*queue = (*queue)[1:]
			return next, nil
		}
	}
	result, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{stubTool{name: "t"}},
	}).Run(context.Background(), Input{
		Messages: user("开始"),
		Steering: take(&steering),
		FollowUp: take(&followUp),
	})
	if err != nil || result.Status != StatusCompleted || result.Turns != 3 {
		t.Fatalf("should run three turns: %+v %v", result, err)
	}
	if got := roles(result.Messages); got != "assistant,tool,user,assistant,user,assistant" {
		t.Fatalf("steering should be injected after tool results and follow-up when the run would end: %s", got)
	}
	requests := client.Requests()
	if last := requests[1].Messages; last[len(last)-1].TextContent() != "改一下方向" {
		t.Fatalf("the second request should end with the steering message: %+v", last)
	}
	if last := requests[2].Messages; last[len(last)-1].TextContent() != "再做一件事" {
		t.Fatalf("the third request should end with the follow-up message: %+v", last)
	}
}

func TestHooks(t *testing.T) {
	client := chattest.NewClient(
		chattest.CallTools(call("1", "echo", `{"v":"原始"}`), call("2", "danger", `{}`)),
		chattest.Say("不应到达"),
	)
	echo := stubTool{name: "echo", run: func(_ *tool.Context, args json.RawMessage) (*tool.Result, error) {
		return tool.Text(string(args)), nil
	}}
	result, err := (&Agent{
		Model:       client,
		Instruction: "系统",
		Tools:       []tool.Tool{echo, stubTool{name: "danger"}},
		Middlewares: []Middleware{
			BeforeModel(func(_ context.Context, turn TurnInfo, req *chat.Request) error {
				req.Model = "切换后的模型"
				req.Messages = req.Messages[:0]
				return nil
			}),
			{
				WrapTool: func(ctx context.Context, inv *ToolInvocation, next ToolNext) (*tool.Result, error) {
					if inv.Call.Name == "danger" {
						return nil, errors.New("需要人工确认")
					}
					inv.Args = []byte(`{"v":"改写"}`)
					result := next(ctx, inv)
					result.Content = append(result.Content, chat.Text("(已审计)"))
					return result, nil
				},
				WrapTurn: func(ctx context.Context, _ TurnInfo, next TurnNext) (*TurnResult, error) {
					result, err := next(ctx)
					if err == nil && len(result.Turn.ToolResults) == 2 {
						result.Status = StatusCompleted
					}
					return result, err
				},
			},
		},
	}).Run(context.Background(), Input{
		Messages: user("执行"),
	})
	if err != nil || result.Status != StatusCompleted || result.Turns != 1 {
		t.Fatalf("WrapTurn should be able to end the run: %+v %v", result, err)
	}
	req := client.Requests()[0]
	if req.Model != "切换后的模型" || len(req.Messages) != 0 {
		t.Fatalf("BeforeModel should be able to rewrite the request: %+v", req)
	}
	if got := roles(result.Messages); got != "assistant,tool,tool" || req.SystemPrompt != "系统" {
		t.Fatalf("wrong run result or system prompt: %s %q", got, req.SystemPrompt)
	}
	texts := toolTexts(result)
	if texts[0] != `{"v":"改写"}(已审计)` || !strings.Contains(texts[1], "需要人工确认") || !result.Messages[2].IsError {
		t.Fatalf("WrapTool should be able to change arguments, rewrite results and intercept: %v", texts)
	}
}

func TestTerminateEndsLoopWithoutAnotherModelCall(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(call("1", "finish", "")), chattest.Say("不应到达"))
	result, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{
			stubTool{name: "finish", run: func(*tool.Context, json.RawMessage) (*tool.Result, error) {
				return &tool.Result{Content: []chat.ContentBlock{chat.Text("done")}, Terminate: true}, nil
			}},
		},
	}).Run(context.Background(), Input{
		Messages: user("结束"),
	})
	if err != nil || result.Status != StatusCompleted || len(client.Requests()) != 1 {
		t.Fatalf("Terminate should end the run directly: %+v %v", result, err)
	}
}

func TestTypedTool(t *testing.T) {
	type params struct {
		Ticker string `json:"ticker" jsonschema:"description=股票代码"`
		Limit  int    `json:"limit,omitempty"`
	}
	quote := tool.New("quote", "查询行情", func(ctx *tool.Context, in params) (*tool.Result, error) {
		if err := ctx.Update(tool.Update{Content: []chat.ContentBlock{chat.Text("查询中")}}); err != nil {
			return nil, err
		}
		return tool.Text(in.Ticker), nil
	}, tool.RunSequentially())

	def := quote.Def()
	schema := string(def.InputSchema)
	if def.Name != "quote" || !strings.Contains(schema, `"required":["ticker"]`) || !strings.Contains(schema, `"description":"股票代码"`) ||
		!quote.(tool.Sequential).Sequential() {
		t.Fatalf("wrong tool definition: %s", schema)
	}

	client := chattest.NewClient(chattest.CallTools(call("1", "quote", `{"ticker":"AAPL"}`), call("2", "quote", `{"limit":1}`)), chattest.Say("完成"))
	var updates int
	result, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{quote},
	}).Run(context.Background(), Input{
		Messages: user("查"),
		Emit: func(_ context.Context, event Event) error {
			if event.Type == EventToolExecutionUpdate {
				updates++
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	texts := toolTexts(result)
	if texts[0] != "AAPL" || !strings.Contains(texts[1], "do not match the schema") || updates != 1 {
		t.Fatalf("should decode arguments and reject a call missing a required field: %v updates=%d", texts, updates)
	}
}

func TestWrapTurnReplacesHistory(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(call("1", "t", "")), chattest.CallTools(call("2", "t", "")), chattest.Say("完成"))
	history := user("开始")
	result, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{stubTool{name: "t"}},
		Middlewares: []Middleware{{WrapTurn: func(ctx context.Context, turn TurnInfo, next TurnNext) (*TurnResult, error) {
			result, err := next(ctx)
			if err != nil || turn.TurnIndex != 0 {
				return result, err
			}
			if roles(result.Messages) != "user,assistant,tool" {
				t.Errorf("the middleware should get the full history including this turn: %s", roles(result.Messages))
			}
			// Simulate compaction: replace the whole history with a summary.
			result.Messages = user("摘要：之前调用过 t")
			return result, nil
		}}},
	}).Run(context.Background(), Input{Messages: history})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("should complete normally: %+v %v", result, err)
	}
	if second := client.Requests()[1]; roles(second.Messages) != "user" || second.Messages[0].TextContent() != "摘要：之前调用过 t" {
		t.Fatalf("the second request should use the replaced history: %+v", second.Messages)
	}
	if third := client.Requests()[2]; roles(third.Messages) != "user,assistant,tool" {
		t.Fatalf("the replaced history should keep accumulating: %s", roles(third.Messages))
	}
	if got := roles(result.Messages); got != "assistant,tool,assistant,tool,assistant" || len(history) != 1 {
		t.Fatalf("Result.Messages should keep every message of the run without modifying the caller's history: %s", got)
	}
}

func TestMiddlewareOrder(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(call("1", "t", "")), chattest.Say("完成"))
	var order []string
	var mu sync.Mutex
	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	layer := func(name string) Middleware {
		return Middleware{
			WrapRun: func(ctx context.Context, _ tool.RunInfo, next RunNext) (*Result, error) {
				record(name + ".run")
				return next(ctx)
			},
			WrapTurn: func(ctx context.Context, turn TurnInfo, next TurnNext) (*TurnResult, error) {
				record(fmt.Sprintf("%s.turn%d", name, turn.TurnIndex))
				return next(ctx)
			},
			WrapModel: func(ctx context.Context, _ TurnInfo, req chat.Request, next chat.StreamFunc) iter.Seq[chat.Event] {
				record(name + ".model")
				return next(ctx, req)
			},
			WrapTool: func(ctx context.Context, inv *ToolInvocation, next ToolNext) (*tool.Result, error) {
				record(name + ".tool")
				return next(ctx, inv), nil
			},
		}
	}
	result, err := (&Agent{
		Model:       client,
		Tools:       []tool.Tool{stubTool{name: "t"}},
		Middlewares: []Middleware{layer("a"), {}, layer("b")},
	}).Run(context.Background(), Input{Messages: user("开始"), Middlewares: []Middleware{layer("in")}})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("should complete normally: %+v %v", result, err)
	}
	want := "a.run,b.run,in.run," +
		"a.turn0,b.turn0,in.turn0,a.model,b.model,in.model,a.tool,b.tool,in.tool," +
		"a.turn1,b.turn1,in.turn1,a.model,b.model,in.model"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("the first middleware should be outermost and run-level middlewares innermost:\n got %s\nwant %s", got, want)
	}
}

func TestWrapTurnRecoversFailedTurn(t *testing.T) {
	client := chattest.NewClient(
		chattest.Fail(&chat.Error{Kind: chat.KindContextLength, Message: "prompt is too long"}),
		chattest.Say("恢复了"),
	)
	var turns []int
	result, err := (&Agent{
		Model: client,
		Middlewares: []Middleware{{WrapTurn: func(ctx context.Context, turn TurnInfo, next TurnNext) (*TurnResult, error) {
			turns = append(turns, turn.TurnIndex)
			result, err := next(ctx)
			if err == nil && result.Status == StatusFailed {
				result.Status = ""
				result.Messages = user("压缩后的历史")
			}
			return result, err
		}}},
	}).Run(context.Background(), Input{Messages: user("问题")})
	if err != nil || result.Status != StatusCompleted || result.Final.TextContent() != "恢复了" {
		t.Fatalf("a recovered failed turn should continue the run: %+v %v", result, err)
	}
	if second := client.Requests()[1]; roles(second.Messages) != "user" || second.Messages[0].TextContent() != "压缩后的历史" {
		t.Fatalf("after recovery the model should be called again with the replaced history: %+v", second.Messages)
	}
	if !slices.Equal(turns, []int{0, 1}) || result.Turns != 2 || roles(result.Messages) != "assistant,assistant" {
		t.Fatalf("the recovered turn counts toward turns with consecutive numbering, and the failed message stays in the result: %v %d %s", turns, result.Turns, roles(result.Messages))
	}
}

func TestWrapTurnCallsNextOnce(t *testing.T) {
	_, err := (&Agent{
		Model: chattest.NewClient(chattest.Say("一"), chattest.Say("二")),
		Middlewares: []Middleware{{WrapTurn: func(ctx context.Context, _ TurnInfo, next TurnNext) (*TurnResult, error) {
			if _, err := next(ctx); err != nil {
				return nil, err
			}
			return next(ctx)
		}}},
	}).Run(context.Background(), Input{Messages: user("问题")})
	if err == nil || !strings.Contains(err.Error(), "can only run once") {
		t.Fatalf("calling next twice in a turn should fail: %v", err)
	}
}

func TestHiddenToolIsNotExecuted(t *testing.T) {
	var executed atomic.Int32
	secret := stubTool{name: "secret", run: func(*tool.Context, json.RawMessage) (*tool.Result, error) {
		executed.Add(1)
		return tool.Text("ok"), nil
	}}
	client := chattest.NewClient(chattest.CallTools(call("1", "secret", ""), call("2", "open", "")), chattest.Say("完成"))
	result, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{secret, stubTool{name: "open"}},
		Middlewares: []Middleware{BeforeModel(func(_ context.Context, _ TurnInfo, req *chat.Request) error {
			req.Tools = req.Tools[1:]
			return nil
		})},
	}).Run(context.Background(), Input{
		Messages: user("执行"),
	})
	texts := toolTexts(result)
	if err != nil || executed.Load() != 0 || !strings.Contains(texts[0], "not available in this turn") || texts[1] != "open result" {
		t.Fatalf("tools not offered to the model should not be executed: executed=%d %v %v", executed.Load(), texts, err)
	}
}

func TestToolTimeout(t *testing.T) {
	var lateUpdate atomic.Value
	release := make(chan struct{})
	stuck := stubTool{name: "stuck", run: func(ctx *tool.Context, _ json.RawMessage) (*tool.Result, error) {
		<-release // a tool that ignores ctx
		lateUpdate.Store(ctx.Update(tool.Update{}) != nil)
		return tool.Text("太迟了"), nil
	}}
	polite := tool.New("polite", "", func(ctx *tool.Context, _ struct{}) (*tool.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, tool.WithTimeout(10*time.Millisecond))
	quick := stubTool{name: "quick"}

	client := chattest.NewClient(chattest.CallTools(call("1", "stuck", ""), call("2", "polite", "{}"), call("3", "quick", "")), chattest.Say("完成"))
	var updates atomic.Int32
	start := time.Now()
	result, err := (&Agent{
		Model:       client,
		Tools:       []tool.Tool{stuck, polite, quick},
		ToolTimeout: 40 * time.Millisecond,
	}).Run(context.Background(), Input{
		Messages: user("执行"),
		Emit: func(_ context.Context, event Event) error {
			if event.Type == EventToolExecutionUpdate {
				updates.Add(1)
			}
			return nil
		},
	})
	elapsed := time.Since(start)
	close(release)
	if err != nil || result.Status != StatusCompleted || elapsed > time.Second {
		t.Fatalf("the loop should continue after a timeout instead of waiting: %+v %v %s", result, err, elapsed)
	}
	texts := toolTexts(result)
	if !strings.Contains(texts[0], "timed out (40ms)") || !strings.Contains(texts[1], "timed out (10ms)") || texts[2] != "quick result" {
		t.Fatalf("wrong timeout results; a timeout declared by the tool should take precedence: %v", texts)
	}
	deadline := time.Now().Add(time.Second)
	for lateUpdate.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if lateUpdate.Load() != true || updates.Load() != 0 {
		t.Fatalf("progress reported after a timeout should be dropped: late=%v updates=%d", lateUpdate.Load(), updates.Load())
	}
}

func TestMaxParallelTools(t *testing.T) {
	var running, peak atomic.Int32
	work := func(*tool.Context, json.RawMessage) (*tool.Result, error) {
		n := running.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		running.Add(-1)
		return tool.Text("ok"), nil
	}
	var calls []chat.ToolCall
	for i := range 6 {
		calls = append(calls, call(string(rune('a'+i)), "work", ""))
	}
	result, err := (&Agent{
		Model:            chattest.NewClient(chattest.CallTools(calls...), chattest.Say("完成")),
		Tools:            []tool.Tool{stubTool{name: "work", run: work}},
		MaxParallelTools: 2,
	}).Run(context.Background(), Input{
		Messages: user("执行"),
	})
	if err != nil || len(toolTexts(result)) != 6 || peak.Load() != 2 {
		t.Fatalf("concurrency should stay within the limit and every tool run: peak=%d %v", peak.Load(), err)
	}
}

func TestToolResultIsTruncated(t *testing.T) {
	long := strings.Repeat("开头", 100) + strings.Repeat("中", 5000) + strings.Repeat("结尾", 100)
	big := stubTool{name: "big", run: func(*tool.Context, json.RawMessage) (*tool.Result, error) {
		return &tool.Result{
			Content: []chat.ContentBlock{chat.Text(long), {Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: make([]byte, 5000)}},
			Details: long,
		}, nil
	}}
	run := func(limit int) chat.Message {
		t.Helper()
		result, err := (&Agent{
			Model:              chattest.NewClient(chattest.CallTools(call("1", "big", "")), chattest.Say("完成")),
			Tools:              []tool.Tool{big},
			MaxToolResultBytes: limit,
		}).Run(context.Background(), Input{
			Messages: user("执行"),
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Messages[1]
	}
	msg := run(1000)
	text := msg.Content[0].Text
	if len(text) > 1200 || !strings.HasPrefix(text, "开头开头") || !strings.HasSuffix(text, "结尾结尾") ||
		!strings.Contains(text, "omitted from the middle") || !utf8.ValidString(text) {
		t.Fatalf("should keep the start and end with a note of the omission: len=%d %q", len(text), text[:60])
	}
	if len(msg.Content[1].Data) != 5000 || msg.Details != long {
		t.Fatal("images and Details should not be truncated")
	}
	if got := run(-1).Content[0].Text; got != long {
		t.Fatal("a negative value should turn truncation off")
	}
	if got := run(0).Content[0].Text; got != long {
		t.Fatal("this result should not be truncated under the default limit")
	}
}

type fakeToolset struct {
	tools        []tool.Tool
	instructions string
	seen         []tool.RunInfo
}

func (f *fakeToolset) Tools(_ context.Context, info tool.RunInfo) ([]tool.Tool, error) {
	f.seen = append(f.seen, info)
	return f.tools, nil
}

func (f *fakeToolset) Instructions(context.Context, tool.RunInfo) (string, error) {
	return f.instructions, nil
}

func TestToolsetsInstructionAndToolContext(t *testing.T) {
	var got *tool.Context
	set := &fakeToolset{instructions: "工具集说明", tools: []tool.Tool{stubTool{name: "from_set", run: func(ctx *tool.Context, _ json.RawMessage) (*tool.Result, error) {
		got = ctx
		return tool.Text("ok"), nil
	}}}}
	client := chattest.NewClient(chattest.CallTools(call("c9", "from_set", "")), chattest.Say("完成"))
	a := &Agent{
		Model:    client,
		Tools:    []tool.Tool{stubTool{name: "fixed"}},
		Toolsets: []tool.Toolset{set},
		InstructionFunc: func(_ context.Context, info tool.RunInfo) (string, error) {
			return "会话 " + info.SessionID + " 的提示词", nil
		},
	}
	info := tool.RunInfo{SessionID: "s1", RunID: "r1"}
	if _, err := a.Run(context.Background(), Input{RunInfo: info, Messages: user("执行")}); err != nil {
		t.Fatal(err)
	}
	requests := client.Requests()
	if requests[0].SystemPrompt != "会话 s1 的提示词\n\n工具集说明" {
		t.Fatalf("the system prompt should come from the function with the toolset instructions appended: %q", requests[0].SystemPrompt)
	}
	if len(requests[0].Tools) != 2 || requests[0].Tools[1].Name != "from_set" {
		t.Fatalf("toolset tools should be added to the request: %+v", requests[0].Tools)
	}
	if len(set.seen) != 1 || set.seen[0].SessionID != info.SessionID || set.seen[0].RunID != info.RunID || set.seen[0].RootSessionID != "s1" {
		t.Fatalf("toolsets should be resolved once at the start of each run: %+v", set.seen)
	}
	if got == nil || got.SessionID != "s1" || got.RunID != "r1" || got.Call.ID != "c9" {
		t.Fatalf("tools should receive the session, run and call info: %+v", got)
	}

	dup := &Agent{Model: client, Tools: []tool.Tool{stubTool{name: "from_set"}}, Toolsets: []tool.Toolset{set}}
	if _, err := dup.Run(context.Background(), Input{Messages: user("执行")}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate tool names should fail: %v", err)
	}
	if _, err := (&Agent{}).Run(context.Background(), Input{Messages: user("x")}); err == nil {
		t.Fatal("a missing Model should fail")
	}
	bad := []chat.Message{{Role: chat.RoleAssistant, Content: []chat.ContentBlock{chat.Text("x")}}}
	if _, err := a.Run(context.Background(), Input{Messages: bad}); err == nil {
		t.Fatal("history ending with an assistant message should fail")
	}
}

// fakeExecutor records the start requests the sub-agent tool hands to the executor. Start runs the sub-agent synchronously so the requests it sees can be checked.
type fakeExecutor struct {
	mu     sync.Mutex
	starts []SubAgentInput
	depths []int
}

func (e *fakeExecutor) Start(ctx context.Context, in SubAgentInput) (tool.Task, error) {
	depth, _ := ctx.Value(depthKey{}).(int)
	e.mu.Lock()
	e.starts = append(e.starts, in)
	e.depths = append(e.depths, depth)
	id := fmt.Sprintf("child-%d", len(e.starts))
	e.mu.Unlock()
	if _, err := in.Agent.Run(ctx, Input{RunInfo: tool.RunInfo{SessionID: id}, Messages: user(in.Task)}); err != nil {
		return tool.Task{}, err
	}
	return tool.Task{ID: id, Type: SubAgentTaskType, SessionID: in.Parent.SessionID, Description: in.Description,
		Status: tool.TaskRunning, Details: SubAgentDetails{Agent: in.Agent.Name, Task: in.Task}}, nil
}

// nopTaskManager only signals that the runtime supports background tasks.
type nopTaskManager struct{}

func (nopTaskManager) Start(context.Context, tool.TaskSpec) (tool.Task, error) {
	return tool.Task{}, nil
}
func (nopTaskManager) List(context.Context) []tool.Task { return nil }
func (nopTaskManager) Get(context.Context, string) (tool.Task, error) {
	return tool.Task{}, tool.ErrTaskNotFound
}
func (nopTaskManager) Stop(context.Context, string) (tool.Task, error) {
	return tool.Task{}, tool.ErrTaskNotFound
}

func TestToolsNeedingTasksAreHiddenWithoutTasks(t *testing.T) {
	for _, withManager := range []bool{false, true} {
		client := chattest.NewClient(chattest.Say("好的"))
		main := &Agent{Model: client, Tools: []tool.Tool{SubAgentTool(SubAgentConfig{})}}
		ctx := WithSubAgentExecutor(context.Background(), &fakeExecutor{})
		if withManager {
			ctx = tool.WithTaskManager(ctx, nopTaskManager{})
		}
		if _, err := main.Run(ctx, Text("开始")); err != nil {
			t.Fatal(err)
		}
		if tools := client.Requests()[0].Tools; (len(tools) == 1) != withManager {
			t.Fatalf("tools that need background tasks should only be offered with a TaskManager (withManager=%v): %+v", withManager, tools)
		}
	}
}

func TestSubAgentTool(t *testing.T) {
	subClient := chattest.NewClient(chattest.Say("营收结论"))
	// A sub-agent tool attached to a specialized sub-agent should have no effect: sub-agents cannot spawn sub-agents.
	researcher := &Agent{Name: "researcher", Description: "负责查资料", Instruction: "你是研究员", Model: subClient,
		Tools: []tool.Tool{SubAgentTool(SubAgentConfig{})}}

	// The main Agent and the general sub-agent share one model and take scripts in call order; the fake executor runs the sub-agent to completion inside Start.
	client := chattest.NewClient(
		chattest.CallTools(call("1", SubAgentToolName, `{"task":"查营收","description":"查营收数据","agent":"researcher"}`)),
		chattest.CallTools(call("2", SubAgentToolName, `{"task":"任务一\n细节"}`)),
		chattest.CallTools(call("g1", "echo", ""), call("g2", "send_email", "")),
		chattest.Say("结论一"),
		chattest.CallTools(call("3", SubAgentToolName, `{"task":"  "}`)),
		chattest.Say("完成"),
	)
	var agentTurns, runTurns atomic.Int32
	var wrapped []string
	var mu sync.Mutex
	main := &Agent{
		Instruction: "你是协调者",
		Model:       client,
		Options:     chat.Options{Output: &chat.OutputFormat{Schema: json.RawMessage(`{"type":"object"}`)}},
		Tools: []tool.Tool{stubTool{name: "echo"}, stubTool{name: "send_email"},
			SubAgentTool(SubAgentConfig{Agents: []*Agent{researcher}, ExcludeTools: []string{"send_email"}})},
		Middlewares: []Middleware{{
			WrapTurn: func(ctx context.Context, _ TurnInfo, next TurnNext) (*TurnResult, error) {
				agentTurns.Add(1)
				return next(ctx)
			},
			WrapTool: func(ctx context.Context, inv *ToolInvocation, next ToolNext) (*tool.Result, error) {
				mu.Lock()
				wrapped = append(wrapped, fmt.Sprintf("%s@%d", inv.Call.Name, inv.Depth))
				mu.Unlock()
				return next(ctx, inv), nil
			},
		}},
	}
	exec := &fakeExecutor{}
	ctx := tool.WithTaskManager(WithSubAgentExecutor(context.Background(), exec), nopTaskManager{})
	result, err := main.Run(ctx, Input{RunInfo: tool.RunInfo{SessionID: "s1"}, Messages: user("开始"),
		Middlewares: []Middleware{{WrapTurn: func(ctx context.Context, _ TurnInfo, next TurnNext) (*TurnResult, error) {
			runTurns.Add(1)
			return next(ctx)
		}}},
	})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("should complete normally: %+v %v", result, err)
	}

	reqs := client.Requests()
	def := reqs[0].Tools[2]
	if len(reqs[0].Tools) != 3 || def.Name != SubAgentToolName || !strings.Contains(def.Description, "researcher: 负责查资料") {
		t.Fatalf("the main Agent should have the sub-agent tool, with specialized sub-agents listed in its description: %+v", reqs[0].Tools)
	}
	subReq := subClient.Requests()[0]
	if subReq.SystemPrompt != "你是研究员\n\n"+subAgentNotes || roles(subReq.Messages) != "user" || subReq.Messages[0].TextContent() != "查营收" || len(subReq.Tools) != 0 {
		t.Fatalf("a specialized sub-agent should start cold, seeing only the task, without the sub-agent tool: %+v", subReq)
	}
	general := reqs[2]
	if general.SystemPrompt != defaultSubAgentInstruction+"\n\n"+subAgentNotes || general.Messages[0].TextContent() != "任务一\n细节" {
		t.Fatalf("the general sub-agent should use its own instruction plus the notes, seeing only the task: %+v", general)
	}
	if reqs[0].Options.Output == nil || general.Options.Output != nil {
		t.Fatalf("the general sub-agent should not inherit the main Agent's output format: %+v %+v", reqs[0].Options.Output, general.Options.Output)
	}
	if len(general.Tools) != 1 || general.Tools[0].Name != "echo" {
		t.Fatalf("the general sub-agent should inherit tools, except excluded ones and the sub-agent tool: %+v", general.Tools)
	}
	if msgs := reqs[3].Messages; !strings.Contains(msgs[len(msgs)-1].TextContent(), "not available in this turn") {
		t.Fatalf("excluded tools should not be executed even if called: %+v", msgs[len(msgs)-1])
	}
	if len(exec.starts) != 2 || exec.starts[0].Parent.SessionID != "s1" || exec.depths[0] != 1 || exec.depths[1] != 1 {
		t.Fatalf("Start should receive the caller's run info, with depth 1 in ctx: %+v %v", exec.starts, exec.depths)
	}
	if exec.starts[0].Description != "查营收数据" || exec.starts[1].Description != "任务一" {
		t.Fatalf("without a description the first line of the task is used: %q %q", exec.starts[0].Description, exec.starts[1].Description)
	}

	texts := toolTexts(result)
	if !strings.Contains(texts[0], "task id: child-1") || !strings.Contains(texts[1], "task id: child-2") || !strings.Contains(texts[2], "task must not be empty") {
		t.Fatalf("should return the sub-task ID immediately and fail on an empty task: %v", texts)
	}
	if task, ok := result.Messages[1].Details.(tool.Task); !ok || task.ID != "child-1" || task.Details.(SubAgentDetails).Agent != "researcher" {
		t.Fatalf("the tool result should carry the sub-task snapshot: %+v", result.Messages[1].Details)
	}
	// The main run has 4 turns. The main Agent's middlewares and the run-level middlewares only apply to the main run; the general sub-agent does not inherit them.
	if agentTurns.Load() != 4 || runTurns.Load() != 4 || slices.ContainsFunc(wrapped, func(w string) bool { return strings.HasSuffix(w, "@1") }) {
		t.Fatalf("the general sub-agent should not inherit the main Agent's middlewares: agentTurns=%d runTurns=%d wrapped=%v", agentTurns.Load(), runTurns.Load(), wrapped)
	}
}

func TestSubAgentExcludeToolsMustExist(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(call("1", SubAgentToolName, `{"task":"做事"}`)), chattest.Say("完成"))
	exec := &fakeExecutor{}
	ctx := tool.WithTaskManager(WithSubAgentExecutor(context.Background(), exec), nopTaskManager{})
	result, err := (&Agent{
		Model: client,
		Tools: []tool.Tool{stubTool{name: "send_email"}, SubAgentTool(SubAgentConfig{ExcludeTools: []string{"send_mail"}})},
	}).Run(ctx, Input{Messages: user("开始")})
	if err != nil || len(exec.starts) != 0 || !strings.Contains(toolTexts(result)[0], `"send_mail" in SubAgentConfig.ExcludeTools is not a tool`) {
		t.Fatalf("a misspelled excluded name should keep the sub-task from starting: %v %v %d", toolTexts(result), err, len(exec.starts))
	}
}

type verdict struct {
	Label string `json:"label" jsonschema:"enum=good,enum=bad"`
	Score int    `json:"score"`
}

func TestRunJSON(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(call("1", "echo", "")), chattest.Say("```json\n{\"label\":\"good\",\"score\":4}\n```"))
	a := &Agent{Model: client, Tools: []tool.Tool{stubTool{name: "echo"}}}
	out, result, err := RunJSON[verdict](context.Background(), a, Text("评价一下"))
	if err != nil || out.Label != "good" || out.Score != 4 || result.Turns != 2 {
		t.Fatalf("should parse the structured result: %+v %+v %v", out, result, err)
	}
	reqs := client.Requests()
	if reqs[0].Output == nil || !strings.Contains(string(reqs[0].Output.Schema), `"enum":["good","bad"]`) || reqs[0].Messages[0].TextContent() != "评价一下" {
		t.Fatalf("should generate the schema from the type and send it with every request: %+v", reqs[0])
	}
	if a.Options.Output != nil {
		t.Fatal("should not modify the caller's Agent definition")
	}

	custom := &chat.OutputFormat{Name: "v", Schema: json.RawMessage(`{"type":"object"}`)}
	client = chattest.NewClient(chattest.Say(`{"label":"任意","score":1}`))
	out, _, err = RunJSON[verdict](context.Background(), &Agent{Model: client, Options: chat.Options{Output: custom}}, Text("x"))
	if err != nil || out.Label != "任意" || client.Requests()[0].Output != custom {
		t.Fatalf("an output format already set should be kept: %+v %v", out, err)
	}
}

func TestRunJSONRepairsInvalidOutputOnce(t *testing.T) {
	usage := func(text string) chattest.Script {
		return func(_ context.Context, b *chat.Builder) {
			b.SetUsage(chat.Usage{TotalTokens: 10})
			b.TextDelta(text)
			b.Done("")
		}
	}
	client := chattest.NewClient(usage(`{"label":"great","score":4}`), usage(`{"label":"good","score":4}`))
	starts := 0
	in := Text("评价一下")
	in.Emit = func(_ context.Context, event Event) error {
		if event.Type == EventAgentStart {
			starts++
		}
		return nil
	}
	out, result, err := RunJSON[verdict](context.Background(), &Agent{Model: client}, in)
	if err != nil || out.Label != "good" {
		t.Fatalf("should get a valid result after the repair: %+v %v", out, err)
	}
	second := client.Requests()[1].Messages
	if roles(second) != "user,assistant,user" || !strings.Contains(second[2].TextContent(), "does not match the schema") {
		t.Fatalf("the repair request should carry the original reply and the validation error: %+v", second)
	}
	if roles(result.Messages) != "assistant,user,assistant" || result.Turns != 2 || result.Usage.TotalTokens != 20 || starts != 2 {
		t.Fatalf("the result should merge both runs: roles=%s turns=%d usage=%+v starts=%d", roles(result.Messages), result.Turns, result.Usage, starts)
	}

	// Still invalid after the repair: invalid_output is returned and the result is still usable.
	client = chattest.NewClient(chattest.Say("不是 JSON"), chattest.Say("还不是"))
	_, result, err = RunJSON[verdict](context.Background(), &Agent{Model: client}, Text("x"))
	var ce *chat.Error
	if !errors.As(err, &ce) || ce.Kind != chat.KindInvalidOutput || len(client.Requests()) != 2 || result.Final.TextContent() != "还不是" {
		t.Fatalf("repair only once and return invalid_output if it still fails: %v %+v", err, result)
	}
}

func TestRunJSONReportsRunsWithoutResult(t *testing.T) {
	var ce *chat.Error
	client := chattest.NewClient(chattest.Fail(&chat.Error{Kind: chat.KindRateLimit, Message: "限流"}))
	_, result, err := RunJSON[verdict](context.Background(), &Agent{Model: client}, Text("x"))
	if !errors.As(err, &ce) || ce.Kind != chat.KindRateLimit || result.Status != StatusFailed || len(client.Requests()) != 1 {
		t.Fatalf("a model failure should return a classified error without a repair: %v %+v", err, result)
	}

	client = chattest.NewClient(func(_ context.Context, b *chat.Builder) {
		b.TextDelta(`{"label":"go`)
		b.Done(chat.StopReasonLength)
	})
	_, _, err = RunJSON[verdict](context.Background(), &Agent{Model: client}, Text("x"))
	if !errors.As(err, &ce) || ce.Kind != chat.KindInvalidOutput || len(client.Requests()) != 1 {
		t.Fatalf("truncated output should not be retried: %v", err)
	}

	client = chattest.NewClient(chattest.CallTools(call("1", "echo", "")))
	_, result, err = RunJSON[verdict](context.Background(), &Agent{Model: client, MaxTurns: 1, Tools: []tool.Tool{stubTool{name: "echo"}}}, Text("x"))
	if !errors.Is(err, ErrMaxTurnsExceeded) || result == nil {
		t.Fatalf("reaching max turns should return ErrMaxTurnsExceeded: %v", err)
	}

	in := Parts(chat.Text("看图"), chat.Media([]byte("%PDF-1.7")))
	if len(in.Messages) != 1 || in.Messages[0].Role != chat.RoleUser || len(in.Messages[0].Content) != 2 {
		t.Fatalf("Parts should build one user message with all content blocks: %+v", in.Messages)
	}
}

func TestStreamSnapshotsShareBytesButStayStable(t *testing.T) {
	image := []byte("生成的图片")
	client := chattest.NewClient(func(_ context.Context, b *chat.Builder) {
		b.Image("image/png", image)
		b.TextDelta("第一段")
		b.TextDelta("第二段")
		b.Done("")
	})
	var kept []*chat.Message
	result, err := (&Agent{Model: client}).Run(context.Background(), Input{
		Messages: user("画一张图"),
		Emit: func(_ context.Context, event Event) error {
			if event.Type == EventMessageUpdate {
				kept = append(kept, event.Message)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, msg := range kept {
		texts = append(texts, msg.TextContent())
		if len(msg.Content) > 0 && &msg.Content[0].Data[0] != &image[0] {
			t.Fatal("snapshots should share media data instead of copying it on every event")
		}
	}
	if got := strings.Join(texts, "|"); !strings.Contains(got, "|第一段|第一段第二段") || texts[0] != "" {
		t.Fatalf("snapshots kept earlier should not change with later output: %s", got)
	}
	if result.Final.TextContent() != "第一段第二段" {
		t.Fatalf("wrong final message: %q", result.Final.TextContent())
	}
}

func TestEventDeltaAccessors(t *testing.T) {
	client := chattest.NewClient(func(_ context.Context, b *chat.Builder) {
		b.ThinkingDelta("想一想")
		b.TextDelta("你")
		b.TextDelta("好")
		b.Done("")
	})
	var text, thinking string
	if _, err := (&Agent{Model: client}).Run(context.Background(), Input{
		Messages: user("hi"),
		Emit: func(_ context.Context, event Event) error {
			text += event.TextDelta()
			thinking += event.ThinkingDelta()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if text != "你好" || thinking != "想一想" {
		t.Fatalf("delta accessors should only return content on the matching events: %q %q", text, thinking)
	}
}

func TestSideCallSendsWhatATurnWouldSend(t *testing.T) {
	client := chattest.NewClient(chattest.CallTools(call("1", "t", "")), chattest.Say("旁路回复"), chattest.Say("完成"))
	var sideCalls []bool
	var sideReply *chat.Message
	var setLate, fromTool error
	a := &Agent{
		Model:   client,
		Options: chat.Options{MaxTokens: 1000},
		Tools: []tool.Tool{stubTool{name: "t", run: func(ctx *tool.Context, _ json.RawMessage) (*tool.Result, error) {
			fromTool = SetMessages(ctx, user("x"))
			return tool.Text("t result"), nil
		}}},
		Middlewares: []Middleware{
			BeforeModel(func(_ context.Context, turn TurnInfo, req *chat.Request) error {
				sideCalls = append(sideCalls, turn.SideCall)
				req.Tools = append(req.Tools, chat.ToolDef{Name: "dynamic", InputSchema: json.RawMessage(`{"type":"object"}`)})
				return nil
			}),
			{WrapTurn: func(ctx context.Context, turn TurnInfo, next TurnNext) (*TurnResult, error) {
				if turn.TurnIndex == 1 {
					var err error
					if sideReply, err = SideCall(ctx, append(user("开始"), chat.NewUserTextMessage("总结一下")), SideCallOptions{MaxTokens: 7}); err != nil {
						return nil, err
					}
					if err := SetMessages(ctx, user("摘要")); err != nil {
						return nil, err
					}
				}
				result, err := next(ctx)
				setLate = SetMessages(ctx, user("太晚了"))
				return result, err
			}},
		},
	}
	var ends int
	result, err := a.Run(context.Background(), Input{Messages: user("开始"), Emit: func(_ context.Context, e Event) error {
		if e.Type == EventMessageEnd && e.Message.Role == chat.RoleAssistant {
			ends++
		}
		return nil
	}})
	if err != nil || result.Status != StatusCompleted || result.Turns != 2 || ends != 2 {
		t.Fatalf("a side call should not count as a turn or emit events: %+v %v ends=%d", result, err, ends)
	}
	if sideReply == nil || sideReply.TextContent() != "旁路回复" || !slices.Equal(sideCalls, []bool{false, true, false}) {
		t.Fatalf("the side call should go through WrapModel marked as a side call: %v %v", sideReply, sideCalls)
	}
	requests := client.Requests()
	first, side := requests[0], requests[1]
	if side.MaxTokens != 7 || side.SystemPrompt != first.SystemPrompt || !slices.EqualFunc(side.Tools, first.Tools, func(a, b chat.ToolDef) bool { return a.Name == b.Name }) ||
		len(side.Tools) != 2 || roles(side.Messages) != "user,user" {
		t.Fatalf("the side call should send a turn's request with its own messages and MaxTokens: %+v", side)
	}
	if got := requests[2].Messages[0].TextContent(); got != "摘要" || roles(requests[2].Messages) != "user" {
		t.Fatalf("SetMessages before next should replace the history of the turn: %s", roles(requests[2].Messages))
	}
	if setLate == nil || fromTool == nil {
		t.Fatalf("SetMessages should fail after next and outside WrapTurn: %v %v", setLate, fromTool)
	}
	if _, err := SideCall(context.Background(), user("x"), SideCallOptions{}); err == nil {
		t.Fatal("SideCall outside a run should fail")
	}
}

func TestAgentSideCallIsARunOfItsOwn(t *testing.T) {
	client := chattest.NewClient(chattest.Say("回复"))
	var order []string
	a := &Agent{Model: client, Instruction: "系统", Middlewares: []Middleware{{
		WrapRun: func(ctx context.Context, _ tool.RunInfo, next RunNext) (*Result, error) {
			order = append(order, "run")
			result, err := next(ctx)
			if result == nil || result.Final == nil || result.Status != StatusCompleted {
				t.Errorf("WrapRun should see the side call's result: %+v", result)
			}
			return result, err
		},
		WrapTurn: func(ctx context.Context, _ TurnInfo, next TurnNext) (*TurnResult, error) {
			order = append(order, "turn")
			return next(ctx)
		},
		WrapModel: func(ctx context.Context, turn TurnInfo, req chat.Request, next chat.StreamFunc) iter.Seq[chat.Event] {
			order = append(order, fmt.Sprintf("model:%v", turn.SideCall))
			return next(ctx, req)
		},
	}}}
	msg, err := a.SideCall(context.Background(), Input{Messages: user("问")}, SideCallOptions{})
	if err != nil || msg.TextContent() != "回复" || strings.Join(order, ",") != "run,model:true" {
		t.Fatalf("a side call outside a run should go through WrapRun and WrapModel only: %v %v %v", msg, err, order)
	}
	if req := client.Requests()[0]; req.SystemPrompt != "系统" {
		t.Fatalf("the side call should resolve the system prompt: %q", req.SystemPrompt)
	}
}
