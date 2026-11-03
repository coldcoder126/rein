package runner

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/chat/chattest"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/skill"
)

// Skills hook into the Agent as a toolset: the catalog goes into the system prompt, and the model loads instructions and reads resources as needed.
// The Runner knows nothing about skills; this checks that the whole chain works.
func TestSkillsThroughToolset(t *testing.T) {
	skills := fstest.MapFS{
		"long-term/SKILL.md":   {Data: []byte("---\nname: long-term\ndescription: 长线分析\n---\n完整的技能说明")},
		"long-term/factors.md": {Data: []byte("因子清单")},
	}
	client := newClient(
		chattest.CallTools(chat.ToolCall{ID: "c1", Name: skill.LoadToolName, Arguments: json.RawMessage(`{"name":"long-term"}`)}),
		chattest.CallTools(chat.ToolCall{ID: "c2", Name: skill.ReadResourceToolName, Arguments: json.RawMessage(`{"name":"long-term","path":"factors.md"}`)}),
		chattest.Say("完成"),
	)
	r, _ := newRunner(t, client, func(c *Config) {
		c.Agent.Instruction = "基础提示"
		c.Agent.Toolsets = []tool.Toolset{skill.NewToolset(skill.FS(skills, "."))}
	})
	if _, err := r.Run(bg, "s", Text("分析一下")); err != nil {
		t.Fatal(err)
	}
	requests := client.Requests()
	prompt := requests[0].SystemPrompt
	if !strings.HasPrefix(prompt, "基础提示\n\n") || !strings.Contains(prompt, "<name>long-term</name>") || strings.Contains(prompt, "完整的技能说明") {
		t.Fatalf("the system prompt should get the skill catalog appended without the full instructions: %q", prompt)
	}
	var names []string
	for _, def := range requests[0].Tools {
		names = append(names, def.Name)
	}
	if got := strings.Join(names, ","); got != "lookup,"+skill.LoadToolName+","+skill.ReadResourceToolName {
		t.Fatalf("skill tools should be added to the tool list: %s", got)
	}
	loaded := requests[1].Messages[len(requests[1].Messages)-1]
	if loaded.IsError || !strings.Contains(loaded.TextContent(), "完整的技能说明") || !strings.Contains(loaded.TextContent(), "factors.md") {
		t.Fatalf("loading a skill should return its instructions and resource list: %+v", loaded)
	}
	if resource := requests[2].Messages[len(requests[2].Messages)-1]; resource.IsError || resource.TextContent() != "因子清单" {
		t.Fatalf("should read the resource content: %+v", resource)
	}

	// Without skills no catalog is appended and no skill tools are provided.
	empty := newClient(chattest.Say("完成"))
	r, _ = newRunner(t, empty, func(c *Config) { c.Agent.Toolsets = []tool.Toolset{skill.NewToolset(skill.Static())} })
	if _, err := r.Run(bg, "s", Text("你好")); err != nil {
		t.Fatal(err)
	}
	if req := empty.Requests()[0]; req.SystemPrompt != "" || len(req.Tools) != 1 {
		t.Fatalf("without skills there should be no skill catalog or tools: %q %d", req.SystemPrompt, len(req.Tools))
	}
}
