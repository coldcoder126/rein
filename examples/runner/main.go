// Runner: sessions that remember earlier turns, with the reply streamed as it is generated.
//
//	ANTHROPIC_API_KEY=... go run ./examples/runner
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/chat/api/anthropic"
	"github.com/coldcoder126/rein/runner"
	"github.com/coldcoder126/rein/session"
)

func main() {
	model, err := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: anthropic.ModelClaudeSonnet5_5})
	if err != nil {
		log.Fatal(err)
	}
	a := &agent.Agent{
		Instruction: "You are a helpful assistant. Keep answers short.",
		Model:       model,
	}
	// The in-memory store is for development; implement session.Store for a database.
	r, err := runner.New(runner.Config{Agent: a, Store: session.NewMemoryStore()})
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	for _, question := range []string{"My name is Ada.", "What is my name?"} {
		fmt.Println("> " + question)
		run, err := r.Start(ctx, "demo-session", runner.Text(question))
		if err != nil {
			log.Fatal(err)
		}
		for event := range run.Events(ctx) {
			fmt.Print(event.TextDelta())
		}
		fmt.Println()
		if _, err := run.Wait(ctx); err != nil {
			log.Fatal(err)
		}
	}
}
