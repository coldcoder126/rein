// MCP: an Agent uses the tools of an MCP server over stdio. To stay self-contained, this program is also the server:
// started with the "serve" argument, it serves one tool built with the official Go SDK.
//
//	ANTHROPIC_API_KEY=... go run ./examples/mcp
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/chat/provider/anthropic"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/mcp"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serve()
		return
	}

	// Stdio takes a function because a new process is started on every (re)connection.
	server := mcp.New("text", mcp.Stdio(func() *exec.Cmd { return exec.Command(os.Args[0], "serve") }))
	defer server.Close()

	a := &agent.Agent{
		Instruction: "You are a helpful assistant.",
		Model:       anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY")}),
		Toolsets:    []tool.Toolset{server}, // the tool is offered to the model as mcp__text__reverse
	}
	result, err := a.Run(context.Background(), agent.Text("Reverse the string 'hello, rein' with the tool."))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Final.TextContent())
}

type reverseArgs struct {
	Text string `json:"text" jsonschema:"the text to reverse"`
}

func serve() {
	s := sdk.NewServer(&sdk.Implementation{Name: "text", Version: "v1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "reverse", Description: "Reverse a string."},
		func(_ context.Context, _ *sdk.CallToolRequest, in reverseArgs) (*sdk.CallToolResult, any, error) {
			runes := []rune(in.Text)
			for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
				runes[i], runes[j] = runes[j], runes[i]
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(runes)}}}, nil, nil
		})
	if err := s.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
