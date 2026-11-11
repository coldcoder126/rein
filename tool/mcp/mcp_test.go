package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

// testServer serves an SDK server over in-memory transports and counts connections and tools/list requests.
type testServer struct {
	t        *testing.T
	server   *sdk.Server
	down     atomic.Bool
	connects atomic.Int32
	lists    atomic.Int32
	ttlMs    atomic.Int32
	sessions []*sdk.ServerSession
}

func newTestServer(t *testing.T, opts *sdk.ServerOptions) *testServer {
	ts := &testServer{t: t, server: sdk.NewServer(&sdk.Implementation{Name: "test", Version: "v1"}, opts)}
	ts.server.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "tools/list" {
				ts.lists.Add(1)
			}
			res, err := next(ctx, method, req)
			if list, ok := res.(*sdk.ListToolsResult); ok {
				list.TTLMs = int(ts.ttlMs.Load())
			}
			return res, err
		}
	})
	return ts
}

func (ts *testServer) transport() Transport {
	return Transport{newTransport: func() sdk.Transport {
		if ts.down.Load() {
			return failingTransport{}
		}
		client, server := sdk.NewInMemoryTransports()
		ss, err := ts.server.Connect(context.Background(), server, nil)
		if err != nil {
			ts.t.Fatal(err)
		}
		ts.connects.Add(1)
		ts.sessions = append(ts.sessions, ss)
		return client
	}}
}

type failingTransport struct{}

func (failingTransport) Connect(context.Context) (sdk.Connection, error) {
	return nil, errors.New("connection refused")
}

func text(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}

func addTool(s *sdk.Server, name string, h sdk.ToolHandler) {
	s.AddTool(&sdk.Tool{Name: name, Description: name + " tool", InputSchema: json.RawMessage(`{"type":"object"}`)}, h)
}

func names(tools []tool.Tool) string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Def().Name)
	}
	return strings.Join(out, ",")
}

func call(t *testing.T, tl tool.Tool, args string) *tool.Result {
	t.Helper()
	result, err := tl.Execute(tool.NewContext(context.Background(), tool.RunInfo{}, chat.ToolCall{}, nil), json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tl.Def().Name, err)
	}
	return result
}

func TestToolsAndCalls(t *testing.T) {
	ts := newTestServer(t, &sdk.ServerOptions{Instructions: "Use echo to repeat text."})
	s := ts.server
	addTool(s, "echo", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var in struct{ Text string }
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return text("echo: " + in.Text), nil
	})
	readOnly := false
	s.AddTool(&sdk.Tool{Name: "files.read", InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &readOnly}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.ImageContent{Data: []byte("\x89PNG\r\n\x1a\n"), MIMEType: "image/png"},
				&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///a.txt", Text: "embedded"}},
				&sdk.ResourceLink{URI: "file:///b.txt", Name: "b.txt"},
			}}, nil
		})
	addTool(s, "stats", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{StructuredContent: map[string]any{"count": 2}}, nil
	})
	addTool(s, "fail", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "bad input"}}, IsError: true}, nil
	})
	addTool(s, "files_read", nil)
	addTool(s, strings.Repeat("x", 60), nil)
	addTool(s, "hidden", nil)

	client := mustNew("my.server", ts.transport(), WithTimeout(3*time.Second),
		WithFilter(func(name string) bool { return name != "hidden" }))
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	tools, err := client.Tools(ctx, tool.RunInfo{})
	cancel() // the session must outlive the run that opened it
	if err != nil {
		t.Fatal(err)
	}
	// The SDK server lists tools sorted by name; "files_read" collides with "files.read" once sanitized.
	if got := names(tools); got != "mcp__my_server__echo,mcp__my_server__fail,mcp__my_server__files_read,mcp__my_server__stats" {
		t.Fatalf("wrong tool names: %s", got)
	}
	instructions, _ := client.Instructions(context.Background(), tool.RunInfo{})
	if !strings.Contains(instructions, `<mcp_server name="my.server">`) || !strings.Contains(instructions, "Use echo to repeat text.") ||
		!strings.Contains(instructions, `Tool "files_read" of this server is unavailable`) || !strings.Contains(instructions, "longer than 64") {
		t.Fatalf("wrong instructions: %s", instructions)
	}
	byName := map[string]*Tool{}
	for _, tl := range tools {
		byName[tl.(*Tool).RemoteName()] = tl.(*Tool)
	}
	if def := byName["echo"].Def(); def.Description != "echo tool" || string(def.InputSchema) != `{"type":"object"}` || byName["echo"].Timeout() != 3*time.Second {
		t.Fatalf("wrong definition: %+v", def)
	}
	if a := byName["echo"].Annotations(); a.ReadOnly || !a.Destructive || !a.OpenWorld {
		t.Fatalf("missing annotations should take the protocol defaults: %+v", a)
	}
	if a := byName["files.read"].Annotations(); !a.ReadOnly || a.Destructive || byName["files.read"].Server() != "my.server" {
		t.Fatalf("wrong annotations: %+v", a)
	}

	if got := call(t, byName["echo"], `{"text":"hi"}`); got.Content[0].Text != "echo: hi" || got.IsError {
		t.Fatalf("wrong echo result: %+v", got)
	}
	if got := call(t, byName["files.read"], ``); len(got.Content) != 3 || !got.Content[0].IsImage() ||
		got.Content[1].Text != "embedded" || !strings.Contains(got.Content[2].Text, "file:///b.txt") {
		t.Fatalf("wrong content conversion: %+v", got.Content)
	}
	if got := call(t, byName["stats"], `{}`); got.Content[0].Text != `{"count":2}` || got.Details.(map[string]any)["count"] != float64(2) {
		t.Fatalf("structured content should go to Details and be mirrored as text: %+v", got)
	}
	if got := call(t, byName["fail"], `{}`); !got.IsError || got.Content[0].Text != "bad input" {
		t.Fatalf("isError should carry over: %+v", got)
	}
}

func TestToolListCache(t *testing.T) {
	ts := newTestServer(t, nil)
	ts.ttlMs.Store(60_000)
	addTool(ts.server, "a", nil)
	client := mustNew("srv", ts.transport())
	defer client.Close()

	run := func() string {
		tools, err := client.Tools(context.Background(), tool.RunInfo{})
		if err != nil {
			t.Fatal(err)
		}
		return names(tools)
	}
	if got := run(); got != "mcp__srv__a" {
		t.Fatalf("wrong tools: %s", got)
	}
	run()
	if ts.lists.Load() != 1 {
		t.Fatalf("the tool list should be cached: lists=%d", ts.lists.Load())
	}
	ts.ttlMs.Store(0)
	addTool(ts.server, "b", nil)
	deadline := time.Now().Add(2 * time.Second)
	for !client.stale.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := run(); got != "mcp__srv__a,mcp__srv__b" || ts.lists.Load() != 2 {
		t.Fatalf("a list-changed notification should refresh the tools: %s lists=%d", got, ts.lists.Load())
	}
	run()
	run()
	if ts.lists.Load() != 4 {
		t.Fatalf("a zero TTL should refetch the list on every run: lists=%d", ts.lists.Load())
	}
}

func TestUnavailableAndReconnect(t *testing.T) {
	ts := newTestServer(t, nil)
	addTool(ts.server, "a", func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return text("ok"), nil })
	ts.down.Store(true)
	client := mustNew("srv", ts.transport())
	defer client.Close()

	tools, err := client.Tools(context.Background(), tool.RunInfo{})
	instructions, _ := client.Instructions(context.Background(), tool.RunInfo{})
	if err != nil || len(tools) != 0 || !strings.Contains(instructions, `status="unavailable"`) || !strings.Contains(instructions, "connection refused") {
		t.Fatalf("an unreachable server should only be reported: %v %v %q", tools, err, instructions)
	}

	ts.down.Store(false)
	tools, err = client.Tools(context.Background(), tool.RunInfo{})
	instructions, _ = client.Instructions(context.Background(), tool.RunInfo{})
	if err != nil || len(tools) != 1 || instructions != "" {
		t.Fatalf("the next run should connect: %v %v %q", tools, err, instructions)
	}

	// The server ends the session: calls fail until the next run reconnects.
	_ = ts.sessions[0].Close()
	deadline := time.Now().Add(2 * time.Second)
	for client.session.Load() != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := tools[0].Execute(tool.NewContext(context.Background(), tool.RunInfo{}, chat.ToolCall{}, nil), nil); err == nil {
		t.Fatal("a call without a connection should fail")
	}
	tools, err = client.Tools(context.Background(), tool.RunInfo{})
	if err != nil || len(tools) != 1 || ts.connects.Load() != 2 {
		t.Fatalf("should reconnect at the start of the next run: %v %v connects=%d", tools, err, ts.connects.Load())
	}
	if got := call(t, tools[0], `{}`); got.Content[0].Text != "ok" {
		t.Fatalf("wrong result after reconnecting: %+v", got)
	}

	_ = client.Close()
	if _, err := client.Tools(context.Background(), tool.RunInfo{}); err == nil {
		t.Fatal("a closed client should fail")
	}
}

func TestWithDeferLoading(t *testing.T) {
	ts := newTestServer(t, nil)
	addTool(ts.server, "a", nil)
	for _, deferred := range []bool{false, true} {
		var opts []Option
		if deferred {
			opts = append(opts, WithDeferLoading())
		}
		client := mustNew("srv", ts.transport(), opts...)
		tools, err := client.Tools(context.Background(), tool.RunInfo{})
		client.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(tools) != 1 || tools[0].Def().Deferred != deferred {
			t.Fatalf("deferred=%v: wrong tools %+v", deferred, tools)
		}
	}
}

// mustNew creates a client for a test whose server name is known to be valid.
func mustNew(name string, transport Transport, opts ...Option) *Client {
	client, err := New(name, transport, opts...)
	if err != nil {
		panic(err)
	}
	return client
}

func TestNewRequiresAName(t *testing.T) {
	if client, err := New(" ", Transport{}); err == nil || client != nil {
		t.Fatalf("New without a server name should fail: %v", err)
	}
}
