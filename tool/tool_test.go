package tool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

type quoteParams struct {
	Ticker string `json:"ticker" jsonschema:"description=股票代码"`
	Limit  int    `json:"limit,omitempty"`
}

func TestNew(t *testing.T) {
	var updates []tool.Update
	quote := tool.New("quote", "查询行情", func(ctx *tool.Context, in quoteParams) (*tool.Result, error) {
		if err := ctx.Update(tool.Update{Details: in.Ticker}); err != nil {
			return nil, err
		}
		return tool.Text(ctx.SessionID + ":" + ctx.Call.ID + ":" + in.Ticker), nil
	}, tool.RunSequentially(), tool.WithTimeout(time.Second))

	def := quote.Def()
	schema := string(def.InputSchema)
	if def.Name != "quote" || def.Description != "查询行情" || !strings.Contains(schema, `"required":["ticker"]`) || !strings.Contains(schema, `"description":"股票代码"`) {
		t.Fatalf("wrong tool definition; omitempty fields should be optional: %s", schema)
	}
	if !quote.(tool.Sequential).Sequential() || quote.(tool.Timeouter).Timeout() != time.Second {
		t.Fatal("options should be exposed through optional interfaces")
	}

	ctx := tool.NewContext(context.Background(), tool.RunInfo{SessionID: "s1"}, chat.ToolCall{ID: "c1"}, func(u tool.Update) error {
		updates = append(updates, u)
		return nil
	})
	result, err := quote.Execute(ctx, json.RawMessage(`{"ticker":"AAPL"}`))
	if err != nil || result.Content[0].Text != "s1:c1:AAPL" || len(updates) != 1 || updates[0].Details != "AAPL" {
		t.Fatalf("should decode arguments and receive the execution context: %+v %v %+v", result, err, updates)
	}
	if _, err := quote.Execute(ctx, json.RawMessage(`{"ticker":1}`)); err == nil {
		t.Fatal("mismatched argument types should return an error")
	}
	// Without a progress callback reports are ignored, so tests can build a context directly.
	if err := tool.NewContext(context.Background(), tool.RunInfo{}, chat.ToolCall{}, nil).Update(tool.Update{}); err != nil {
		t.Fatal(err)
	}
	if r := tool.Error("失败"); !r.IsError || r.Content[0].Text != "失败" {
		t.Fatalf("Error should build an error result: %+v", r)
	}
}
