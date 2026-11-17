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
	model, err := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: anthropic.Model{Name: "claude-sonnet-5-5"}})
	if err != nil {
		log.Fatal(err)
	}
	a := &agent.Agent{
		Instruction: "You coordinate research. Delegate independent pieces of work to sub-agents in parallel, then combine their results.",
		Model:       model,
		// Sub-agents run as background tasks; the task tool lets the model list and stop them.
		Tools: []tool.Tool{agent.SubAgentTool(agent.SubAgentConfig{}), task.Tool()},
	}

	// A result that arrives while the session is idle wakes the Agent with it; results arriving during a run are injected into that run.
	r, err := runner.New(runner.Config{
		Agent: a,
		Store: session.NewMemoryStore(),
		OnTaskFinished: func(_ context.Context, t tool.Task, delivery runner.TaskResultDelivery) {
			fmt.Printf("[task %s %s, %s]\n", t.Description, t.Status, delivery)
		},
		TaskWake: func(context.Context, tool.Task) ([]runner.RunOption, bool) { return nil, true },
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, sessionID := context.Background(), "demo-session"
	run, err := r.Start(ctx, sessionID, runner.Text("Give me one strength and one weakness each of Go and Rust for writing web services. "+
		"Research each language with its own sub-agent."))
	if err != nil {
		log.Fatal(err)
	}
	// Follow the first run and every run the results wake, until no task is left and the session is idle.
	// A task leaves the task table only after its result has been delivered, so a woken run is already current by then.
	for run != nil {
		result, err := run.Wait(ctx)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(result.Final.Text())
		for run = r.Current(sessionID); run == nil && len(r.ListTasks(sessionID)) > 0; run = r.Current(sessionID) {
			time.Sleep(100 * time.Millisecond)
		}
	}
}
