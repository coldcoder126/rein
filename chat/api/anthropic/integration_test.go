package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
)

// Real model integration tests: they need REIN_INTEGRATION=1 and take the key from ANTHROPIC_API_KEY or env.yaml.
func integrationClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real model integration tests in short mode")
	}
	if os.Getenv("REIN_INTEGRATION") != "1" {
		t.Skip("set REIN_INTEGRATION=1 to run real model integration tests")
	}
	cfg.APIKey = chattest.EnvKey(t, "ANTHROPIC_API_KEY", "claude-api-key")
	cfg.Timeout = 120 * time.Second
	return mustNew(cfg)
}

// mustDone runs one request, checks the stream contract and requires a normal finish.
func mustDone(t *testing.T, client *Client, req chat.Request) chat.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	events := chattest.Record(client.Stream(ctx, req))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	final := events[len(events)-1]
	if final.Type != chat.EventDone {
		t.Fatalf("should finish normally: %v", final.Err)
	}
	return final.Message.Clone()
}

func TestIntegrationStreamText(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelClaudeSonnet5_5})
	msg := mustDone(t, client, chat.Request{
		Options:  chat.Options{MaxTokens: 512},
		Messages: []chat.Message{chat.NewUserTextMessage("请原样输出这个单词，不要输出别的内容：pong")},
	})
	if !strings.Contains(strings.ToLower(msg.TextContent()), "pong") {
		t.Fatalf("unexpected reply: %q", msg.TextContent())
	}
	if msg.Usage.TotalTokens == 0 || msg.ResponseID == "" || msg.StopReason != chat.StopReasonStop {
		t.Fatalf("should return usage, the response ID and the stop reason: %+v", msg)
	}
}

// Every model generation should accept the thinking parameters, including with temperature set, thinking off, and the default behavior.
func TestIntegrationReasoningLevels(t *testing.T) {
	cases := []struct {
		model string
		level chat.ReasoningLevel
	}{
		{ModelClaudeSonnet5_5, ""},
		{ModelClaudeSonnet5_5, chat.ReasoningOff},
		{ModelClaudeSonnet5_5, chat.ReasoningXHigh},
		{ModelClaudeHaiku5_5, chat.ReasoningOff},
		{ModelClaudeHaiku5_5, chat.ReasoningLow},
		{ModelClaudeSonnet4_6, chat.ReasoningXHigh},
		{ModelClaudeHaiku4_5, chat.ReasoningOff},
		{ModelClaudeHaiku4_5, chat.ReasoningMedium},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+string(tc.level), func(t *testing.T) {
			client := integrationClient(t, Config{Model: tc.model})
			msg := mustDone(t, client, chat.Request{
				Options:  chat.Options{MaxTokens: 2048, Reasoning: tc.level, Temperature: chat.Float(1)},
				Messages: []chat.Message{chat.NewUserTextMessage("17 乘以 23 等于多少？只回答数字。")},
			})
			if !strings.Contains(msg.TextContent(), "391") {
				t.Fatalf("unexpected reply: %q", msg.TextContent())
			}
		})
	}
}

// Two rounds of tool calls: the second must send back the first round's thinking blocks and signatures unchanged, or the server rejects it.
// Covers both adaptive and budget thinking.
func TestIntegrationThinkingToolReplay(t *testing.T) {
	for _, model := range []string{ModelClaudeSonnet5_5, ModelClaudeHaiku4_5} {
		t.Run(model, func(t *testing.T) {
			client := integrationClient(t, Config{Model: model})
			req := chat.Request{
				SystemPrompt: "你是测试助手。需要数据时必须先调用工具，不要编造。",
				Options:      chat.Options{MaxTokens: 4096, Reasoning: chat.ReasoningHigh},
				Tools: []chat.ToolDef{{
					Name:        "get_metric",
					Description: "查询公司指标，返回固定夹具数据",
					InputSchema: json.RawMessage(`{"type":"object","properties":{"ticker":{"type":"string"},"metric":{"type":"string"}},"required":["ticker","metric"],"additionalProperties":false}`),
				}},
				Messages: []chat.Message{chat.NewUserTextMessage("有三家公司：AAPL、MSFT、NVDA。把它们的代码按字母倒序排列后取中间那个，再把它的每个字母在字母表中的序号相加；" +
					"如果和是偶数就查它的 revenue_growth，否则查它的 net_margin。请调用 get_metric 查询，再用一句话回答，回答必须包含工具返回的数值。")},
			}

			first := mustDone(t, client, req)
			calls := first.ToolCalls()
			if first.StopReason != chat.StopReasonToolUse || len(calls) != 1 || calls[0].Name != "get_metric" {
				t.Fatalf("the first round should call the tool: %+v", first)
			}
			thinking := 0
			for _, block := range first.Content {
				if block.Type == chat.ContentTypeThinking {
					thinking++
					if block.Signature == "" {
						t.Fatalf("thinking blocks should be signed: %+v", block)
					}
				}
			}
			t.Logf("thinking blocks in the first round: %d", thinking)

			req.Messages = append(req.Messages, first, chat.Message{
				Role: chat.RoleTool, ToolCallID: calls[0].ID, ToolName: calls[0].Name,
				Content: []chat.ContentBlock{chat.Text(`{"value":"7.31%"}`)},
			})
			second := mustDone(t, client, req)
			if !strings.Contains(second.TextContent(), "7.31") {
				t.Fatalf("the second round should cite the tool result: %q", second.TextContent())
			}
		})
	}
}

func TestIntegrationImageInput(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelClaudeSonnet5_5})
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	msg := mustDone(t, client, chat.Request{
		Options: chat.Options{MaxTokens: 512},
		Messages: []chat.Message{{Role: chat.RoleUser, Content: []chat.ContentBlock{
			chat.Text("这张图是什么颜色？只回答一个英文单词。"),
			{Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: buf.Bytes()},
		}}},
	})
	if !strings.Contains(strings.ToLower(msg.TextContent()), "red") {
		t.Fatalf("unexpected reply: %q", msg.TextContent())
	}
}

// A second request with the same prefix should hit the cache; with caching off there should be no cache reads or writes.
func TestIntegrationPromptCache(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelClaudeHaiku5_5})
	// Add a timestamp to avoid hitting a cache left by a previous test run.
	system := "测试运行 " + time.Now().Format(time.RFC3339Nano) + "\n" +
		strings.Repeat("这是一段用于填充提示词缓存的背景资料，内容本身没有意义。", 300)
	req := chat.Request{
		SystemPrompt: system,
		Options:      chat.Options{MaxTokens: 64},
		Messages:     []chat.Message{chat.NewUserTextMessage("只回复：ok")},
	}

	first := mustDone(t, client, req)
	if first.Usage.CacheWrite == 0 {
		t.Fatalf("the first request should write the cache: %+v", first.Usage)
	}
	req.Messages = append(req.Messages, first, chat.NewUserTextMessage("再回复一次：ok"))
	second := mustDone(t, client, req)
	if second.Usage.CacheRead == 0 {
		t.Fatalf("the second request should hit the cache: %+v", second.Usage)
	}
	t.Logf("first %+v\nsecond %+v", first.Usage, second.Usage)

	req.CacheRetention = chat.CacheRetentionNone
	req.SystemPrompt = "无缓存 " + system
	none := mustDone(t, client, req)
	if none.Usage.CacheWrite != 0 || none.Usage.CacheRead != 0 {
		t.Fatalf("there should be no cache reads or writes with caching off: %+v", none.Usage)
	}
}

type cityInfo struct {
	Name      string   `json:"name" jsonschema:"description=城市的英文名"`
	Country   string   `json:"country"`
	Landmarks []string `json:"landmarks" jsonschema:"description=恰好两个著名地标"`
	Coastal   bool     `json:"coastal"`
}

func checkCity(t *testing.T, city cityInfo, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(city.Name), "paris") || city.Country == "" || len(city.Landmarks) != 2 || city.Coastal {
		t.Fatalf("unexpected structured result: %+v", city)
	}
}

var cityRequest = chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("给出法国首都的信息。")}}

// Structured output: native constraints with thinking on, confirming through an HTTP hook that the request body carries the schema.
func TestIntegrationStructuredOutput(t *testing.T) {
	for _, model := range []string{ModelClaudeSonnet5_5, ModelClaudeHaiku4_5} {
		t.Run(model, func(t *testing.T) {
			var payload string
			client := integrationClient(t, Config{Model: model, HTTPHooks: chat.HTTPHooks{
				OnPayload: func(_ context.Context, body []byte) []byte { payload = string(body); return nil },
			}})
			req := cityRequest
			req.Reasoning = chat.ReasoningLow
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			city, _, err := chat.CompleteJSON[cityInfo](ctx, client, req)
			checkCity(t, city, err)
			if !strings.Contains(payload, `"json_schema"`) {
				t.Fatalf("the request body should contain the native json_schema format: %s", payload)
			}
		})
	}
}

var toolChoiceModels = []string{ModelClaudeSonnet5_5, ModelClaudeHaiku5_5, ModelClaudeSonnet4_6, ModelClaudeHaiku4_5}

// Tool choice constraints: required, none, and a specific tool.
func TestIntegrationToolChoice(t *testing.T) {
	tools := []chat.ToolDef{
		{Name: "get_weather", Description: "查询天气", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)},
		{Name: "get_time", Description: "查询当前时间", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)},
	}
	cases := []struct {
		name   string
		prompt string
		choice chat.ToolChoice
		check  func(calls []chat.ToolCall) bool
	}{
		{"required", "你好", chat.ToolChoice{Mode: chat.ToolChoiceRequired}, func(c []chat.ToolCall) bool { return len(c) > 0 }},
		{"none", "北京天气怎么样？", chat.ToolChoice{Mode: chat.ToolChoiceNone}, func(c []chat.ToolCall) bool { return len(c) == 0 }},
		{"tool", "告诉我北京现在的天气和时间。", chat.ForceTool("get_time"), func(c []chat.ToolCall) bool { return len(c) > 0 && c[0].Name == "get_time" }},
	}
	for _, model := range toolChoiceModels {
		for _, tc := range cases {
			for _, level := range []chat.ReasoningLevel{"", chat.ReasoningHigh} {
				t.Run(model+"/"+tc.name+"/"+string(level), func(t *testing.T) {
					client := integrationClient(t, Config{Model: model})
					req := chat.Request{
						Options:  chat.Options{ToolChoice: tc.choice, Reasoning: level},
						Tools:    tools,
						Messages: []chat.Message{chat.NewUserTextMessage(tc.prompt)},
					}
					if model == ModelClaudeSonnet5_5 && tc.name != "none" {
						// Models that cannot turn thinking off do not support forced tool calls and should get a clear local error.
						_, err := chat.Complete(context.Background(), client, req)
						if ce := chat.AsError(err); ce == nil || ce.Kind != chat.KindInvalidRequest || !strings.Contains(err.Error(), "does not support forced tool calls") {
							t.Fatalf("should return invalid_request: %v", err)
						}
						return
					}
					msg := mustDone(t, client, req)
					if !tc.check(msg.ToolCalls()) {
						t.Fatalf("tool calls do not match the constraint: %+v text=%q", msg.ToolCalls(), msg.TextContent())
					}
				})
			}
		}
	}
}
