package skill_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/skill"
)

var ctx = context.Background()

func memFS() fstest.MapFS {
	return fstest.MapFS{
		"group/pdf/SKILL.md":           {Data: []byte("---\nname: pdf\ndescription: 处理 PDF：提取文本和表格\n---\n\n# 用法\n先读 reference.md。\n")},
		"group/pdf/reference.md":       {Data: []byte("参考资料正文")},
		"group/pdf/scripts/extract.py": {Data: []byte("print('hi')")},
		"group/pdf/logo.png":           {Data: []byte{0xff, 0xfe, 0x00}},
		"group/pdf/nested/SKILL.md":    {Data: []byte("嵌套技能，没有 frontmatter")},
		"group/pdf/nested/notes.md":    {Data: []byte("嵌套技能的资源")},
		"plain/SKILL.md":               {Data: []byte("没有 frontmatter 的技能")},
	}
}

func TestFSSource(t *testing.T) {
	src := skill.FS(memFS(), ".")
	infos, err := src.List(ctx)
	if err != nil || len(infos) != 3 || infos[0].Name != "nested" || infos[1].Name != "pdf" || infos[2].Name != "plain" {
		t.Fatalf("should find all skills recursively, sorted by name, using the directory name when there is no name: %+v %v", infos, err)
	}
	if infos[1].Description != "处理 PDF：提取文本和表格" {
		t.Fatalf("a colon in description should not cut it off: %q", infos[1].Description)
	}
	s, err := src.Load(ctx, "pdf")
	if err != nil || !strings.HasPrefix(s.Instructions, "# 用法") {
		t.Fatalf("should load the skill instructions: %+v %v", s, err)
	}
	if got := strings.Join(s.Resources, ","); got != "logo.png,reference.md,scripts/extract.py" {
		t.Fatalf("resources should be the other files in the skill directory, excluding nested skills: %s", got)
	}
	if data, err := src.ReadResource(ctx, "pdf", "scripts/extract.py"); err != nil || string(data) != "print('hi')" {
		t.Fatalf("should read the resource content: %q %v", data, err)
	}
	for _, bad := range []string{"../../plain/SKILL.md", "/etc/passwd", "missing.md", "."} {
		if _, err := src.ReadResource(ctx, "pdf", bad); !errors.Is(err, skill.ErrNotFound) {
			t.Fatalf("an out-of-bounds or missing path %q should return ErrNotFound: %v", bad, err)
		}
	}
	if _, err := src.Load(ctx, "missing"); !errors.Is(err, skill.ErrNotFound) {
		t.Fatalf("a missing skill should return ErrNotFound: %v", err)
	}
}

func execute(t *testing.T, tools []tool.Tool, name, args string) (*tool.Result, error) {
	t.Helper()
	for _, tl := range tools {
		if tl.Def().Name == name {
			return tl.Execute(tool.NewContext(ctx, tool.RunInfo{}, chat.ToolCall{Name: name}, nil), json.RawMessage(args))
		}
	}
	t.Fatalf("no %s in the toolset", name)
	return nil, nil
}

func TestToolset(t *testing.T) {
	ts := skill.NewToolset(skill.FS(memFS(), "."))
	instructions, err := ts.(tool.Instructor).Instructions(ctx, tool.RunInfo{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<available_skills>", "<name>pdf</name>", "处理 PDF：提取文本和表格", skill.LoadToolName} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("catalog missing %s: %s", want, instructions)
		}
	}
	if strings.Contains(instructions, "# 用法") {
		t.Fatalf("the catalog should not contain the full skill instructions: %s", instructions)
	}

	tools, err := ts.Tools(ctx, tool.RunInfo{})
	if err != nil || len(tools) != 2 {
		t.Fatalf("should provide the two tools for loading skills and reading resources: %d %v", len(tools), err)
	}
	result, err := execute(t, tools, skill.LoadToolName, `{"name":"pdf"}`)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if !strings.HasPrefix(text, `<skill name="pdf">`) || !strings.Contains(text, "# 用法") ||
		!strings.Contains(text, "reference.md\nscripts/extract.py") || result.Details.(map[string]any)["skill"] != "pdf" {
		t.Fatalf("the load result should contain the instructions and the resource list: %q", text)
	}
	if result, err := execute(t, tools, skill.ReadResourceToolName, `{"name":"pdf","path":"reference.md"}`); err != nil || result.Content[0].Text != "参考资料正文" {
		t.Fatalf("should read the resource content: %+v %v", result, err)
	}
	if _, err := execute(t, tools, skill.ReadResourceToolName, `{"name":"pdf","path":"logo.png"}`); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary resources should give a clear error: %v", err)
	}
	if _, err := execute(t, tools, skill.ReadResourceToolName, `{"name":"pdf","path":"../x"}`); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("a missing resource should fail: %v", err)
	}
	_, err = execute(t, tools, skill.LoadToolName, `{"name":"missing"}`)
	if err == nil || !strings.Contains(err.Error(), `unknown skill "missing"`) || !strings.Contains(err.Error(), "available skills: nested, pdf, plain") {
		t.Fatalf("an unknown skill should list the available ones so the model can correct itself: %v", err)
	}
}

func TestStaticSourceAndEmptyToolset(t *testing.T) {
	src := skill.Static(skill.Skill{Name: "b", Description: "B"}, skill.Skill{Name: "a", Instructions: "A 的说明", Resources: []string{"x"}})
	infos, _ := src.List(ctx)
	if len(infos) != 2 || infos[0].Name != "a" {
		t.Fatalf("the catalog should be sorted by name: %+v", infos)
	}
	if s, err := src.Load(ctx, " a "); err != nil || s.Instructions != "A 的说明" || len(s.Resources) != 0 {
		t.Fatalf("should get the skill; in-memory skills have no resources: %+v %v", s, err)
	}

	empty := skill.NewToolset(skill.Static())
	tools, _ := empty.Tools(ctx, tool.RunInfo{})
	instructions, _ := empty.(tool.Instructor).Instructions(ctx, tool.RunInfo{})
	if len(tools) != 0 || instructions != "" {
		t.Fatalf("no tools or instructions should be provided without skills: %d %q", len(tools), instructions)
	}
}
