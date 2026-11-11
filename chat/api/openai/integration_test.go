package openai

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
	"github.com/coldcoder126/rein/chat/imageproc"
)

// Real model integration tests: they need REIN_INTEGRATION=1 and take the key from OPENAI_API_KEY or env.yaml.
func integrationClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real model integration tests in short mode")
	}
	if os.Getenv("REIN_INTEGRATION") != "1" {
		t.Skip("set REIN_INTEGRATION=1 to run real model integration tests")
	}
	cfg.APIKey = chattest.EnvKey(t, "OPENAI_API_KEY", "openai-api-key")
	cfg.Timeout = 120 * time.Second
	if m := os.Getenv("REIN_OPENAI_MODEL"); m != "" {
		cfg.Model = m
	}
	return mustNew(cfg)
}

func TestIntegrationStreamText(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelGPT6Luna})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	events := chattest.Record(client.Stream(ctx, chat.Request{
		Options:  chat.Options{MaxTokens: 512},
		Messages: []chat.Message{chat.NewUserTextMessage("只回复一个英文单词：pong")},
	}))
	if err := chattest.CheckStream(events); err != nil {
		t.Fatal(err)
	}
	final := events[len(events)-1]
	if final.Type != chat.EventDone {
		t.Fatalf("should finish normally: %+v err=%v", final.Message.ErrorMessage, final.Err)
	}
	if !strings.Contains(strings.ToLower(final.Message.TextContent()), "pong") {
		t.Fatalf("unexpected reply: %q", final.Message.TextContent())
	}
	if final.Message.Usage.TotalTokens == 0 || final.Message.ResponseID == "" {
		t.Fatalf("should return usage and the response ID: %+v", final.Message)
	}
}

// Two rounds of tool calls: the second must send back the first round's reasoning items, message ids and function_call ids unchanged;
// the server rejects any signature replayed wrongly.
func TestIntegrationToolCallReplay(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelGPT6Luna, Reasoning: true})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	tools := []chat.ToolDef{{
		Name:        "get_metric",
		Description: "查询公司指标，返回固定夹具数据",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"ticker":{"type":"string"},"metric":{"type":"string"}},"required":["ticker","metric"],"additionalProperties":false}`),
	}}
	history := []chat.Message{chat.NewUserTextMessage("请先调用 get_metric 查询 AAPL 的 revenue_growth，再用一句话回答，回答必须包含工具返回的数值。")}
	req := chat.Request{
		SystemPrompt: "你是测试助手。需要数据时必须先调用工具，不要编造。",
		Options:      chat.Options{MaxTokens: 4096},
		Tools:        tools,
	}

	req.Messages = history
	first, err := chat.Complete(ctx, client, req)
	if err != nil {
		t.Fatalf("first round failed: %v", err)
	}
	calls := first.ToolCalls()
	if len(calls) == 0 {
		t.Fatalf("the first round should request the tool: %+v", first.Content)
	}
	t.Logf("content blocks in the first round: %d, signed thinking: %v", len(first.Content), hasSignedThinking(first))
	history = append(history, *first)
	for _, call := range calls {
		history = append(history, chat.Message{
			Role: chat.RoleTool, ToolCallID: call.ID, ToolName: call.Name,
			Content: []chat.ContentBlock{chat.Text(`{"ticker":"AAPL","metric":"revenue_growth","value":"6.2%"}`)},
		})
	}

	req.Messages = history
	second, err := chat.Complete(ctx, client, req)
	if err != nil {
		t.Fatalf("second round (replaying the first round's reasoning items and item ids) failed: %v", err)
	}
	if !strings.Contains(second.TextContent(), "6.2") {
		t.Fatalf("the final answer should contain the value returned by the tool: %q", second.TextContent())
	}
}

func hasSignedThinking(m *chat.Message) bool {
	for _, b := range m.Content {
		if b.Type == chat.ContentTypeThinking && b.Signature != "" {
			return true
		}
	}
	return false
}

func TestIntegrationImageInput(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelGPT6Luna})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	img := image.NewNRGBA(image.Rect(0, 0, 96, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 96; x++ {
			img.Set(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	res, err := imageproc.Process(buf.Bytes(), client.Info().ImageLimits)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := chat.Complete(ctx, client, chat.Request{
		Options: chat.Options{MaxTokens: 1024},
		Messages: []chat.Message{{
			Role:    chat.RoleUser,
			Content: append([]chat.ContentBlock{chat.Text("这张图片是什么纯色？只用一个英文单词回答。")}, res.Blocks()...),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(msg.TextContent()), "red") {
		t.Fatalf("should recognize red: %q", msg.TextContent())
	}
}

func TestIntegrationImageGeneration(t *testing.T) {
	if os.Getenv("REIN_INTEGRATION_IMAGEGEN") != "1" {
		t.Skip("set REIN_INTEGRATION_IMAGEGEN=1 to run (image generation is billed)")
	}
	client := integrationClient(t, Config{Model: ModelGPT6Luna, ImageGeneration: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	msg, err := chat.Complete(ctx, client, chat.Request{
		Options:  chat.Options{MaxTokens: 2048},
		Messages: []chat.Message{chat.NewUserTextMessage("请生成一张纯蓝色的小方块图片。")},
	})
	if err != nil {
		t.Fatal(err)
	}
	generated := false
	for _, b := range msg.Content {
		if b.Type == chat.ContentTypeMedia {
			if imageproc.DetectMIME(b.Data) == "" {
				t.Fatalf("the generated image should be a valid image: mime=%q", b.MIMEType)
			}
			generated = true
		}
	}
	if !generated {
		t.Fatalf("no image in the reply: %+v", msg.Content)
	}

	// Replay the generated image as history (an image_generation_call item), ask about its color, and forbid generating again.
	follow, err := chat.Complete(ctx, client, chat.Request{
		Options: chat.Options{MaxTokens: 1024},
		Messages: []chat.Message{
			chat.NewUserTextMessage("请生成一张纯蓝色的小方块图片。"), *msg,
			chat.NewUserTextMessage("不要再生成图片。你刚才生成的图片是什么颜色？只用一个英文单词回答。"),
		},
	})
	if err != nil {
		t.Fatalf("second round replaying the generated image failed: %v", err)
	}
	t.Logf("follow-up reply: %q", follow.TextContent())
}

// The second round sends back the first round's reasoning item (with encrypted content) and message id unchanged; acceptance by the server proves stateless replay works.
func TestIntegrationReasoningReplay(t *testing.T) {
	client := integrationClient(t, Config{Model: ModelGPT6Luna, Reasoning: true})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	history := []chat.Message{chat.NewUserTextMessage("一个房间里有 3 个开关对应隔壁房间 3 盏灯，你只能进隔壁房间一次，如何确定对应关系？请仔细推理后给出答案。")}
	first, err := chat.Complete(ctx, client, chat.Request{Options: chat.Options{MaxTokens: 8000}, Messages: history})
	if err != nil {
		t.Fatalf("first round failed: %v", err)
	}
	if !hasSignedThinking(first) {
		t.Skipf("the model returned no reasoning item this time, cannot test replay: %+v", first.Content)
	}

	history = append(history, *first, chat.NewUserTextMessage("请用不超过 20 个字总结你的方法。"))
	second, err := chat.Complete(ctx, client, chat.Request{Options: chat.Options{MaxTokens: 8000}, Messages: history})
	if err != nil {
		t.Fatalf("second round (replaying the reasoning item and message id) failed: %v", err)
	}
	if strings.TrimSpace(second.TextContent()) == "" {
		t.Fatalf("the second round should reply: %+v", second.Content)
	}
	t.Logf("second round reply: %s", second.TextContent())
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

// Structured output: native constraints, and prompt injection plus local validation for endpoints without native constraints.
func TestIntegrationStructuredOutput(t *testing.T) {
	for name, cfg := range map[string]Config{"native": {}, "fallback": {Endpoint: Endpoint{Compat: Compat{NoStructuredOutput: true}}}} {
		t.Run(name, func(t *testing.T) {
			var payload string
			cfg.HTTPHooks = chat.HTTPHooks{OnPayload: func(_ context.Context, body []byte) []byte { payload = string(body); return nil }}
			client := integrationClient(t, cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			city, _, err := chat.CompleteJSON[cityInfo](ctx, client, cityRequest)
			checkCity(t, city, err)
			if native := strings.Contains(payload, `"type":"json_schema"`); native != (name == "native") {
				t.Fatalf("the request body does not use the expected method: %s", payload)
			}
		})
	}
}

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
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := integrationClient(t, Config{Model: ModelGPT6Luna})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			msg, err := chat.Complete(ctx, client, chat.Request{
				Options:  chat.Options{ToolChoice: tc.choice},
				Tools:    tools,
				Messages: []chat.Message{chat.NewUserTextMessage(tc.prompt)},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.check(msg.ToolCalls()) {
				t.Fatalf("tool calls do not match the constraint: %+v text=%q", msg.ToolCalls(), msg.TextContent())
			}
		})
	}
}
