// Quickstart: an Agent with one typed tool, run directly without sessions.
//
//	ANTHROPIC_API_KEY=... go run ./examples/quickstart
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/coldcoder126/rein/agent"
	"github.com/coldcoder126/rein/chat/api/anthropic"
	"github.com/coldcoder126/rein/tool"
)

type weatherArgs struct {
	City string `json:"city" jsonschema:"description=City name"`
}

func main() {
	weather := tool.New("get_weather", "Get the current weather of a city.",
		func(ctx *tool.Context, in weatherArgs) (*tool.Result, error) {
			return tool.Text(in.City + ": sunny, 22°C"), nil
		})

	model, err := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: anthropic.ModelClaudeSonnet5_5})
	if err != nil {
		log.Fatal(err)
	}
	a := &agent.Agent{
		Instruction: "You are a helpful assistant.",
		Model:       model,
		Tools:       []tool.Tool{weather},
	}
	result, err := a.Run(context.Background(), agent.Text("What's the weather in Paris?"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Final.TextContent())
}
