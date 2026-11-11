package gemini

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

// Real model integration tests: they need REIN_INTEGRATION=1.
// The key comes from GEMINI_API_KEY / gemini-api-key in env.yaml for the Gemini API,
// or else from GEMINI_VERTEX_API_KEY / gemini-vertex-api-key for Vertex.
func integrationClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real model integration tests in short mode")
	}
	if os.Getenv("REIN_INTEGRATION") != "1" {
		t.Skip("set REIN_INTEGRATION=1 to run real model integration tests")
	}
	if key := chattest.LookupKey("GEMINI_API_KEY", "gemini-api-key"); key != "" {
		cfg.APIKey = key
	} else {
		cfg.APIKey = chattest.EnvKey(t, "GEMINI_VERTEX_API_KEY", "gemini-vertex-api-key")
		cfg.Vertex = true
	}
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

func redPNG(t *testing.T) []byte {
	t.Helper()
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
	return buf.Bytes()
}

var metricTool = []chat.ToolDef{{
	Name:        "get_metric",
	Description: "查询公司指标，返回固定夹具数据",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"ticker":{"type":"string"},"metric":{"type":"string"}},"required":["ticker","metric"],"additionalProperties":false}`),
}}

func TestIntegrationStreamText(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelGemini3_5Flash})
	msg := mustDone(t, client, chat.Request{
		Options:  chat.Options{MaxTokens: 512},
		Messages: []chat.Message{chat.NewUserTextMessage("请原样输出这个单词，不要输出别的内容：pong")},
	})
	if !strings.Contains(strings.ToLower(msg.TextContent()), "pong") {
		t.Fatalf("unexpected reply: %q", msg.TextContent())
	}
	if msg.Usage.TotalTokens == 0 || msg.Usage.Input == 0 || msg.ResponseID == "" || msg.StopReason != chat.StopReasonStop {
		t.Fatalf("should return usage, the response ID and the stop reason: %+v", msg)
	}
}

// Every model generation should accept the thinking parameters.
func TestIntegrationReasoningLevels(t *testing.T) {
	for _, model := range []string{ModelGemini3_5Flash, ModelGemini3_1ProPreview, ModelGemini2_5Flash} {
		for _, level := range []chat.ReasoningLevel{"", chat.ReasoningOff, chat.ReasoningLow, chat.ReasoningXHigh} {
			t.Run(model+"/"+string(level), func(t *testing.T) {
				client := integrationClient(t, Config{Model: model})
				msg := mustDone(t, client, chat.Request{
					Options:  chat.Options{Reasoning: level, Temperature: chat.Float(1)},
					Messages: []chat.Message{chat.NewUserTextMessage("17 乘以 23 等于多少？只回答数字。")},
				})
				if !strings.Contains(msg.TextContent(), "391") {
					t.Fatalf("unexpected reply: %q", msg.TextContent())
				}
			})
		}
	}
}

// Two rounds of tool calls: the second must send the first round's thoughtSignature back on its original part.
func TestIntegrationThinkingToolReplay(t *testing.T) {
	for _, model := range []string{ModelGemini3_5Flash, ModelGemini2_5Flash} {
		t.Run(model, func(t *testing.T) {
			client := integrationClient(t, Config{Model: model})
			req := chat.Request{
				SystemPrompt: "你是测试助手。需要数据时必须先调用工具，不要编造。",
				Options:      chat.Options{Reasoning: chat.ReasoningHigh},
				Tools:        metricTool,
				Messages:     []chat.Message{chat.NewUserTextMessage("请同时查询 AAPL 和 MSFT 的 revenue_growth（两次 get_metric 调用），再用一句话回答，回答必须包含工具返回的两个数值。")},
			}
			first := mustDone(t, client, req)
			calls := first.ToolCalls()
			if first.StopReason != chat.StopReasonToolUse || len(calls) == 0 {
				t.Fatalf("the first round should call the tool: %+v", first)
			}
			var kinds []string
			for _, block := range first.Content {
				kinds = append(kinds, string(block.Type)+map[bool]string{true: "+sig", false: ""}[block.Signature != ""])
			}
			t.Logf("content blocks in the first round: %v", kinds)

			req.Messages = append(req.Messages, first)
			for _, call := range calls {
				value := `{"value":"7.31%"}`
				if strings.Contains(string(call.Arguments), "MSFT") {
					value = `{"value":"9.42%"}`
				}
				req.Messages = append(req.Messages, chat.Message{
					Role: chat.RoleTool, ToolCallID: call.ID, ToolName: call.Name,
					Content: []chat.ContentBlock{chat.Text(value)},
				})
			}
			second := mustDone(t, client, req)
			for len(second.ToolCalls()) > 0 {
				// Keep feeding results when the model calls tools in two steps.
				req.Messages = append(req.Messages, second)
				for _, call := range second.ToolCalls() {
					req.Messages = append(req.Messages, chat.Message{
						Role: chat.RoleTool, ToolCallID: call.ID, ToolName: call.Name,
						Content: []chat.ContentBlock{chat.Text(`{"value":"9.42%"}`)},
					})
				}
				second = mustDone(t, client, req)
			}
			if !strings.Contains(second.TextContent(), "7.31") || !strings.Contains(second.TextContent(), "9.42") {
				t.Fatalf("the last round should cite the tool result: %q", second.TextContent())
			}
		})
	}
}

// Tool calls left by other models have no thought signature and should not be rejected when replayed to Gemini.
func TestIntegrationForeignToolHistory(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelGemini3_5Flash})
	msg := mustDone(t, client, chat.Request{
		Tools: metricTool,
		Messages: []chat.Message{
			chat.NewUserTextMessage("查询 AAPL 的 revenue_growth 并告诉我数值。"),
			{
				Role: chat.RoleAssistant, Provider: "openai", Model: "gpt", StopReason: chat.StopReasonToolUse,
				Content: []chat.ContentBlock{{Type: chat.ContentTypeToolCall, ToolCall: &chat.ToolCall{
					ID: "call_abc", Name: "get_metric", Arguments: json.RawMessage(`{"ticker":"AAPL","metric":"revenue_growth"}`),
				}}},
			},
			{Role: chat.RoleTool, ToolCallID: "call_abc", ToolName: "get_metric", Content: []chat.ContentBlock{chat.Text(`{"value":"7.31%"}`)}},
		},
	})
	if !strings.Contains(msg.TextContent(), "7.31") {
		t.Fatalf("should cite the tool result: %q", msg.TextContent())
	}
}

func TestIntegrationImageInput(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelGemini3_5Flash})
	msg := mustDone(t, client, chat.Request{
		Messages: []chat.Message{{Role: chat.RoleUser, Content: []chat.ContentBlock{
			chat.Text("这张图是什么颜色？只回答一个英文单词。"),
			{Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: redPNG(t)},
		}}},
	})
	if !strings.Contains(strings.ToLower(msg.TextContent()), "red") {
		t.Fatalf("unexpected reply: %q", msg.TextContent())
	}
}

// Images in tool results: embedded in functionResponse on Gemini 3, separate parts on 2.x.
func TestIntegrationToolResultImage(t *testing.T) {
	for _, model := range []string{ModelGemini3_5Flash, ModelGemini2_5Flash} {
		t.Run(model, func(t *testing.T) {
			client := integrationClient(t, Config{Model: model})
			req := chat.Request{
				Tools:    []chat.ToolDef{{Name: "screenshot", Description: "截取当前屏幕并把图片返回给你，你可以直接看到图片内容", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}},
				Messages: []chat.Message{chat.NewUserTextMessage("调用 screenshot 截图，然后只用一个英文单词回答屏幕是什么颜色。")},
			}
			first := mustDone(t, client, req)
			calls := first.ToolCalls()
			if len(calls) != 1 {
				t.Fatalf("should call the tool once: %+v", first)
			}
			req.Messages = append(req.Messages, first, chat.Message{
				Role: chat.RoleTool, ToolCallID: calls[0].ID, ToolName: calls[0].Name,
				Content: []chat.ContentBlock{{Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: redPNG(t)}},
			})
			second := mustDone(t, client, req)
			if !strings.Contains(strings.ToLower(second.TextContent()), "red") {
				t.Fatalf("should recognize the image returned by the tool: %q", second.TextContent())
			}
		})
	}
}

func TestIntegrationStructuredOutput(t *testing.T) {
	for _, model := range []string{ModelGemini3_5Flash, ModelGemini2_5Flash} {
		t.Run(model, func(t *testing.T) {
			client := integrationClient(t, Config{Model: model})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			city, _, err := chat.CompleteJSON[cityInfo](ctx, client, cityRequest)
			checkCity(t, city, err)
		})
	}
}

// Generate an image and replay it in the next turn. Needs an image model, set with REIN_GEMINI_IMAGE_MODEL.
func TestIntegrationImageGeneration(t *testing.T) {
	model := os.Getenv("REIN_GEMINI_IMAGE_MODEL")
	if model == "" {
		t.Skip("set REIN_GEMINI_IMAGE_MODEL to run the image generation test")
	}
	client := integrationClient(t, Config{Model: model, ImageGeneration: true})
	req := chat.Request{Messages: []chat.Message{chat.NewUserTextMessage("生成一张纯蓝色的正方形图片。")}}
	first := mustDone(t, client, req)
	images := 0
	for _, block := range first.Content {
		if block.Type == chat.ContentTypeMedia && len(block.Data) > 0 {
			images++
		}
	}
	if images == 0 {
		t.Fatalf("should generate an image: %+v", first.StopReason)
	}
	req.Messages = append(req.Messages, first, chat.NewUserTextMessage("你刚才生成的图片主要是什么颜色？只回答一个英文单词。"))
	second := mustDone(t, client, req)
	if !strings.Contains(strings.ToLower(second.TextContent()), "blue") {
		t.Fatalf("should see the image generated in the previous turn: %q", second.TextContent())
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

// Excludes gemini-3.5-flash: in testing, forced tool calls (mode=ANY) on Vertex intermittently hang on the server,
// regardless of the thinking config; other models are fine.
var toolChoiceModels = []string{"gemini-3-flash-preview", ModelGemini2_5Flash}

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
					msg := mustDone(t, client, chat.Request{
						Options:  chat.Options{ToolChoice: tc.choice, Reasoning: level},
						Tools:    tools,
						Messages: []chat.Message{chat.NewUserTextMessage(tc.prompt)},
					})
					if !tc.check(msg.ToolCalls()) {
						t.Fatalf("tool calls do not match the constraint: %+v text=%q", msg.ToolCalls(), msg.TextContent())
					}
				})
			}
		}
	}
}
