// Approval: a WrapTool middleware asks a human before every shell command runs.
//
//	ANTHROPIC_API_KEY=... go run ./examples/approval
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/chat/provider/anthropic"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/shell"
)

func main() {
	stdin := bufio.NewReader(os.Stdin)
	var mu sync.Mutex // tools may run in parallel; ask one question at a time
	approve := agent.Middleware{
		WrapTool: func(ctx context.Context, inv *agent.ToolInvocation, next agent.ToolNext) (*tool.Result, error) {
			if inv.Call.Name != shell.ToolName {
				return next(ctx, inv), nil
			}
			mu.Lock()
			fmt.Printf("Run %s? [y/N] ", inv.Args)
			answer, _ := stdin.ReadString('\n')
			mu.Unlock()
			if strings.TrimSpace(strings.ToLower(answer)) != "y" {
				return tool.Error("The user declined to run this command."), nil
			}
			return next(ctx, inv), nil
		},
	}

	a := &agent.Agent{
		Instruction: "You are a helpful assistant with access to a shell.",
		Model:       anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: anthropic.ModelClaudeSonnet5_5}),
		// shell.Local runs commands on this machine; use an isolated Executor in production.
		Tools:       []tool.Tool{shell.Tool(shell.Config{Executor: shell.Local{}})},
		Middlewares: []agent.Middleware{approve},
	}
	result, err := a.Run(context.Background(), agent.Text("How many Go files are in the current directory tree?"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Final.TextContent())
}
