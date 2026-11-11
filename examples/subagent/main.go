// Sub-agents: the main Agent delegates work to sub-agents running in the background and combines their results.
//
//	ANTHROPIC_API_KEY=... go run ./examples/subagent
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/chat/api/anthropic"
	"github.com/coldcoder126/rein/runner"
	"github.com/coldcoder126/rein/session"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/task"
)

func main() {
	model, err := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: anthropic.ModelClaudeSonnet5_5})
	if err != nil {
		log.Fatal(err)
	}
	a := &agent.Agent{
		Instruction: "You coordinate research. Delegate independent pieces of work to sub-agents in parallel, then combine their results.",
		Model:       model,
		// Sub-agents run as background tasks; the task tool lets the model list and stop them.
		Tools: []tool.Tool{agent.SubAgentTool(agent.SubAgentConfig{}), task.Tool()},
	}

	// A result that arrives while the session is idle is only written to history; the business decides when the Agent continues.
	saved := make(chan tool.Task, 16)
	r, err := runner.New(runner.Config{
		Agent: a,
		Store: session.NewMemoryStore(),
		OnTaskFinished: func(_ context.Context, t tool.Task, delivery runner.TaskResultDelivery) {
			fmt.Printf("[task %s %s]\n", t.Description, t.Status)
			if delivery == runner.ResultSavedToHistory {
				saved <- t
			}
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, sessionID := context.Background(), "demo-session"
	input := runner.Text("Give me one strength and one weakness each of Go and Rust for writing web services. Research each language with its own sub-agent.")
	for {
		run, err := r.Start(ctx, sessionID, input)
		if err != nil {
			log.Fatal(err)
		}
		result, err := run.Wait(ctx)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(result.Final.Text())

		// Wait for the remaining sub-agents, then let the Agent continue with the results saved to history.
		n := 0
		for len(r.ListTasks(sessionID)) > 0 {
			select {
			case <-saved:
				n++
			case <-time.After(100 * time.Millisecond):
			}
		}
		for len(saved) > 0 {
			<-saved
			n++
		}
		if n == 0 {
			return
		}
		input = runner.Text("All sub-agent results are in. Continue.")
	}
}
