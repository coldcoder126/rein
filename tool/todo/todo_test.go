package todo_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/todo"
)

func execute(t *testing.T, args string) *tool.Result {
	t.Helper()
	result, err := todo.Tool().Execute(tool.NewContext(context.Background(), tool.RunInfo{}, chat.ToolCall{}, nil), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWriteNormalizesList(t *testing.T) {
	result := execute(t, `{"todos":[
		{"content":" 读代码 ","status":"completed"},
		{"content":"  ","status":"pending"},
		{"content":"改实现","status":"in_progress"},
		{"content":"跑测试","status":"in_progress"}
	]}`)
	want := []todo.Item{
		{Content: "读代码", Status: todo.Completed},
		{Content: "改实现", Status: todo.InProgress},
		{Content: "跑测试", Status: todo.Pending},
	}
	if !reflect.DeepEqual(result.Details, want) {
		t.Fatalf("should trim whitespace, drop empty items and keep only the first in-progress item: %+v", result.Details)
	}
	if got := result.Content[0].Text; got != "Todo list updated: 1/3 completed. In progress: 改实现." {
		t.Fatalf("the result should state the progress briefly: %q", got)
	}
}

func TestWriteEmptyListClears(t *testing.T) {
	result := execute(t, `{"todos":[]}`)
	if items := result.Details.([]todo.Item); len(items) != 0 || result.Content[0].Text != "Todo list cleared." {
		t.Fatalf("an empty list means clearing it: %+v %q", items, result.Content[0].Text)
	}
}

func TestSchemaRestrictsStatus(t *testing.T) {
	schema := string(todo.Tool().Def().InputSchema)
	if !strings.Contains(schema, `"enum":["pending","in_progress","completed"]`) || !strings.Contains(schema, `"required":["todos"]`) {
		t.Fatalf("the schema should restrict status values and require todos: %s", schema)
	}
}
