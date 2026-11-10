package tool_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

type slowTool struct{}

func (slowTool) Def() chat.ToolDef { return chat.ToolDef{Name: "slow"} }
func (slowTool) Execute(*tool.Context, json.RawMessage) (*tool.Result, error) {
	return tool.Text("done"), nil
}
func (slowTool) Sequential() bool       { return true }
func (slowTool) Timeout() time.Duration { return time.Minute }
func (slowTool) NeedsTaskManager() bool { return true }

type instructedSet struct{}

func (instructedSet) Tools(context.Context, tool.RunInfo) ([]tool.Tool, error) {
	return []tool.Tool{slowTool{}}, nil
}
func (instructedSet) Instructions(context.Context, tool.RunInfo) (string, error) {
	return "use slow", nil
}

func TestDeferKeepsTheToolAndItsOptionalInterfaces(t *testing.T) {
	d := tool.Defer(slowTool{})
	if def := d.Def(); def.Name != "slow" || !def.Deferred {
		t.Fatalf("wrong definition: %+v", def)
	}
	if s, ok := d.(tool.Sequential); !ok || !s.Sequential() {
		t.Fatal("Sequential should pass through")
	}
	if to, ok := d.(tool.Timeouter); !ok || to.Timeout() != time.Minute {
		t.Fatal("Timeouter should pass through")
	}
	if n, ok := d.(tool.NeedsTaskManager); !ok || !n.NeedsTaskManager() {
		t.Fatal("NeedsTaskManager should pass through")
	}
	res, err := d.Execute(tool.NewContext(context.Background(), tool.RunInfo{}, chat.ToolCall{}, nil), nil)
	if err != nil || res.Content[0].Text != "done" {
		t.Fatalf("Execute should run the tool: %v %v", res, err)
	}
	if plain := tool.Defer(tool.New("plain", "", func(*tool.Context, quoteParams) (*tool.Result, error) { return nil, nil })); plain.(tool.Sequential).Sequential() {
		t.Fatal("a tool without Sequential stays concurrent")
	}
}

func TestDeferAll(t *testing.T) {
	set := tool.DeferAll(instructedSet{})
	tools, err := set.Tools(context.Background(), tool.RunInfo{})
	if err != nil || len(tools) != 1 || !tools[0].Def().Deferred {
		t.Fatalf("every tool should be deferred: %+v %v", tools, err)
	}
	text, err := set.(tool.Instructor).Instructions(context.Background(), tool.RunInfo{})
	if err != nil || text != "use slow" {
		t.Fatalf("instructions should pass through: %q %v", text, err)
	}
}
