package chat_test

import (
	"strings"
	"testing"
	"time"

	"github.com/coldcoder126/rein/chat"
)

func toolCallBlock(id, name string) chat.ContentBlock {
	return chat.ContentBlock{Type: chat.ContentTypeToolCall, ToolCall: &chat.ToolCall{ID: id, Name: name, Arguments: []byte(`{}`)}}
}

func assistant(provider, model string, blocks ...chat.ContentBlock) chat.Message {
	return chat.Message{Role: chat.RoleAssistant, Provider: provider, Model: model, Content: blocks, StopReason: chat.StopReasonStop}
}

func toolResult(id string) chat.Message {
	return chat.Message{Role: chat.RoleTool, ToolCallID: id, ToolName: "lookup", Content: []chat.ContentBlock{chat.Text("ok")}}
}

var targetModel = chat.ModelInfo{Provider: "anthropic", Model: "claude"}

func TestNormalizeDropsFailedAssistantMessages(t *testing.T) {
	failed := assistant("anthropic", "claude", chat.Text("半截"))
	failed.StopReason = chat.StopReasonError
	aborted := assistant("anthropic", "claude", toolCallBlock("c1", "lookup"))
	aborted.StopReason = chat.StopReasonAborted

	out := chat.NormalizeForModel([]chat.Message{
		chat.NewUserTextMessage("一"), failed, chat.NewUserTextMessage("二"), aborted,
	}, targetModel)
	if len(out) != 2 || out[0].TextContent() != "一" || out[1].TextContent() != "二" {
		t.Fatalf("should drop failed and aborted assistant messages: %+v", out)
	}
}

func TestNormalizeClosesOrphanToolCalls(t *testing.T) {
	out := chat.NormalizeForModel([]chat.Message{
		chat.NewUserTextMessage("查"),
		assistant("anthropic", "claude", toolCallBlock("c1", "lookup"), toolCallBlock("c2", "lookup")),
		toolResult("c1"),
		chat.NewUserTextMessage("继续"),
	}, targetModel)

	if len(out) != 5 {
		t.Fatalf("should add the missing tool result: %d", len(out))
	}
	filled := out[3]
	if filled.Role != chat.RoleTool || filled.ToolCallID != "c2" || !filled.IsError || filled.TextContent() != "No result provided" {
		t.Fatalf("wrong filled tool result: %+v", filled)
	}
	if out[4].Role != chat.RoleUser {
		t.Fatalf("the filled result should come before the next user message: %+v", out)
	}
}

func TestNormalizeClosesTrailingToolCallsAndDropsOrphanResults(t *testing.T) {
	out := chat.NormalizeForModel([]chat.Message{
		toolResult("ghost"),
		chat.NewUserTextMessage("查"),
		assistant("anthropic", "claude", toolCallBlock("c1", "lookup")),
	}, targetModel)
	if len(out) != 3 || out[2].Role != chat.RoleTool || out[2].ToolCallID != "c1" {
		t.Fatalf("orphan results should be dropped and the trailing unfinished call filled: %+v", out)
	}
}

func TestNormalizeCrossModelThinking(t *testing.T) {
	thinking := chat.ContentBlock{Type: chat.ContentTypeThinking, Text: "推理", Signature: "sig"}
	redacted := chat.ContentBlock{Type: chat.ContentTypeThinking, Redacted: true, Signature: "enc"}
	empty := chat.ContentBlock{Type: chat.ContentTypeThinking, Text: "  "}
	msgs := func(provider, model string) []chat.Message {
		return []chat.Message{chat.NewUserTextMessage("q"), assistant(provider, model, thinking, redacted, empty, chat.Text("答"))}
	}

	same := chat.NormalizeForModel(msgs("anthropic", "claude"), targetModel)[1]
	if len(same.Content) != 3 || same.Content[0].Signature != "sig" || !same.Content[1].Redacted {
		t.Fatalf("the same model should keep signed and redacted thinking and drop empty blocks: %+v", same.Content)
	}

	other := chat.NormalizeForModel(msgs("openai", "gpt"), targetModel)[1]
	if len(other.Content) != 2 || other.Content[0].Type != chat.ContentTypeText || other.Content[0].Text != "推理" {
		t.Fatalf("across models thinking should become text and redacted thinking be dropped: %+v", other.Content)
	}

	otherAPI := targetModel
	otherAPI.API = "openai-responses"
	if got := chat.NormalizeForModel(msgs("anthropic", "claude"), otherAPI)[1]; got.Content[0].Type != chat.ContentTypeText {
		t.Fatalf("the same provider and model over another API should not replay signatures: %+v", got.Content)
	}
}

func TestNormalizeToolCallIDOnlyForOtherModels(t *testing.T) {
	fix := chat.WithToolCallIDNormalizer(func(id string) string { return strings.ReplaceAll(id, "|", "_") })
	build := func(provider string) []chat.Message {
		return []chat.Message{
			chat.NewUserTextMessage("q"),
			assistant(provider, "any", toolCallBlock("a|b", "lookup")),
			toolResult("a|b"),
		}
	}

	other := chat.NormalizeForModel(build("openai"), targetModel, fix)
	if other[1].ToolCalls()[0].ID != "a_b" || other[2].ToolCallID != "a_b" {
		t.Fatalf("call IDs from other models and their results should be normalized: %+v", other)
	}

	sameTarget := chat.ModelInfo{Provider: "openai", Model: "any"}
	same := chat.NormalizeForModel(build("openai"), sameTarget, fix)
	if same[1].ToolCalls()[0].ID != "a|b" {
		t.Fatalf("the same model should keep its call IDs: %+v", same)
	}
}

func TestNormalizeReplacesMediaForTextOnlyEndpoint(t *testing.T) {
	image := chat.ContentBlock{Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: []byte{1}}
	pdf := chat.Media([]byte("%PDF-1.7")).WithName("报告.pdf")
	user := chat.Message{Role: chat.RoleUser, Content: []chat.ContentBlock{chat.Text("看图"), image, pdf}}

	out := chat.NormalizeForModel([]chat.Message{user}, targetModel)
	if len(out[0].Content) != 3 || !strings.Contains(out[0].Content[1].Text, "image/png") || !strings.Contains(out[0].Content[2].Text, "报告.pdf, application/pdf") {
		t.Fatalf("a text-only endpoint should replace each media block with a note: %+v", out[0].Content)
	}
	if user.Content[1].Type != chat.ContentTypeMedia {
		t.Fatal("should not modify the input")
	}

	media := targetModel
	media.SupportsMedia = true
	odd := chat.Media([]byte{0, 1, 2}).WithName("数据.bin")
	got := chat.NormalizeForModel([]chat.Message{{Role: chat.RoleUser, Content: []chat.ContentBlock{image, pdf, odd}}}, media)
	for i, block := range got[0].Content {
		if block.Type != chat.ContentTypeMedia {
			t.Fatalf("an endpoint with media support should not filter by type, block %d changed: %+v", i, block)
		}
	}
}

func TestNormalizeUsesFilesOnlyWhereTheyCanBeRead(t *testing.T) {
	target := chat.ModelInfo{Provider: "anthropic", Model: "claude", SupportsMedia: true, FileScope: "anthropic"}
	file := func(scope string, expires time.Time) chat.FileRef {
		return chat.FileRef{Scope: scope, ID: "file_1", ExpiresAt: expires}
	}
	withURL := chat.MediaURL("https://example.com/a.pdf")
	cases := []struct {
		name  string
		block chat.ContentBlock
		want  func(chat.ContentBlock) bool
	}{
		{"same scope keeps the file", withURL.WithFile(file("anthropic", time.Time{})),
			func(b chat.ContentBlock) bool { return b.File != nil && b.File.ID == "file_1" }},
		{"not yet expiring keeps the file", withURL.WithFile(file("anthropic", time.Now().Add(time.Hour))),
			func(b chat.ContentBlock) bool { return b.File != nil }},
		{"another scope falls back to the URL", withURL.WithFile(file("openai", time.Time{})),
			func(b chat.ContentBlock) bool { return b.File == nil && b.URL == withURL.URL }},
		{"about to expire falls back to the URL", withURL.WithFile(file("anthropic", time.Now().Add(time.Minute))),
			func(b chat.ContentBlock) bool { return b.File == nil && b.URL == withURL.URL }},
		{"no fallback becomes a note", chat.MediaFile(file("openai", time.Time{}), "application/pdf").WithName("a.pdf"),
			func(b chat.ContentBlock) bool {
				return b.Type == chat.ContentTypeText && strings.Contains(b.Text, "not available to this model: a.pdf, application/pdf")
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []chat.Message{{Role: chat.RoleUser, Content: []chat.ContentBlock{tc.block}}, assistant("anthropic", "claude",
				toolCallBlock("c1", "read")), {Role: chat.RoleTool, ToolCallID: "c1", Content: []chat.ContentBlock{tc.block}}}
			out := chat.NormalizeForModel(msgs, target)
			if !tc.want(out[0].Content[0]) || !tc.want(out[2].Content[0]) {
				t.Fatalf("user message and tool result should both resolve the file: %+v / %+v", out[0].Content[0], out[2].Content[0])
			}
		})
	}
	if out := chat.NormalizeForModel([]chat.Message{{Role: chat.RoleUser, Content: []chat.ContentBlock{withURL.WithFile(file("", time.Time{}))}}},
		chat.ModelInfo{SupportsMedia: true}); out[0].Content[0].File != nil {
		t.Fatal("an endpoint without a file store should never use files")
	}
}

func TestNormalizeGeneratedImagesInAssistantMessages(t *testing.T) {
	image := chat.ContentBlock{Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: []byte{1}, Signature: "sig"}
	history := func() []chat.Message {
		return []chat.Message{
			chat.NewUserTextMessage("画一只猫"),
			assistant("google", "img-model", chat.Text("好的"), image, image, chat.Text("完成")),
		}
	}

	// The target model cannot replay generated images: consecutive images merge into one placeholder.
	out := chat.NormalizeForModel(history(), chat.ModelInfo{Provider: "anthropic", Model: "claude"})
	got := out[1].Content
	if len(got) != 3 || got[1].Type != chat.ContentTypeText || !strings.Contains(got[1].Text, "generated image omitted") {
		t.Fatalf("images should become a placeholder without image replay: %+v", got)
	}

	// Same model: keep the images and signatures.
	same := chat.NormalizeForModel(history(), chat.ModelInfo{Provider: "google", Model: "img-model", ImageOutput: true})
	if len(same[1].Content) != 4 || same[1].Content[1].Signature != "sig" {
		t.Fatalf("the same model should keep images and signatures: %+v", same[1].Content)
	}

	// Another model with image output: keep the images, but signatures are only valid for the original model.
	other := chat.NormalizeForModel(history(), chat.ModelInfo{Provider: "google", Model: "other-img", ImageOutput: true})
	if len(other[1].Content) != 4 || other[1].Content[1].Type != chat.ContentTypeMedia || other[1].Content[1].Signature != "" {
		t.Fatalf("across models images should be kept without signatures: %+v", other[1].Content)
	}
	if image.Signature != "sig" {
		t.Fatal("should not modify the input")
	}
}

func TestImageLimitsWithDefaults(t *testing.T) {
	got := chat.ImageLimits{MaxWidth: 800}.WithDefaults()
	if got.MaxWidth != 800 || got.MaxHeight != chat.DefaultImageLimits.MaxHeight ||
		got.MaxBytes != chat.DefaultImageLimits.MaxBytes || got.JPEGQuality != chat.DefaultImageLimits.JPEGQuality {
		t.Fatalf("zero fields should use the defaults: %+v", got)
	}
}
