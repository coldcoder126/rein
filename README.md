# rein

[![CI](https://github.com/coldcoder126/rein/actions/workflows/ci.yml/badge.svg)](https://github.com/coldcoder126/rein/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/coldcoder126/rein.svg)](https://pkg.go.dev/github.com/coldcoder126/rein)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

English | [简体中文](README.zh-CN.md)

rein is a Go framework for building LLM agents: the agent loop, tools, sessions and the runtime around them, with Anthropic models as the first-class target.

> **Status:** v0.x. The API may change in incompatible ways before v1. Requires Go 1.26+.

## Features

- **Model providers**: Anthropic first, plus OpenAI, Gemini and DeepSeek behind one interface: messages, streaming events, classified errors, structured output, and media input inline, by URL or as uploaded files.
- **Agent loop**: parallel tool execution with timeouts and result truncation, steering and follow-up messages during a run, and a middleware chain wrapping runs, turns, model calls and tool calls.
- **Tools**: typed tools from a parameter struct, with the schema generated for you; toolsets that provide tools per session; deferred tools found through the provider's tool search (Anthropic, OpenAI) without breaking the prompt cache.
- **Sub-agents and background tasks**: delegate work to sub-agents running in the background; run shell commands in the background too.
- **Runner**: sessions persisted through a store interface, background runs that any number of views can follow and rejoin, message queues, automatic context compaction, and the model chosen per run.
- **Built-in tools**: skills, a checklist, shell, background task management, and MCP servers.

## Install

```sh
go get github.com/coldcoder126/rein
```

## Quick start

```go
type weatherArgs struct {
	City string `json:"city" jsonschema:"description=City name"`
}

weather := tool.New("get_weather", "Get the current weather of a city.",
	func(ctx *tool.Context, in weatherArgs) (*tool.Result, error) {
		return tool.Text(in.City + ": sunny, 22°C"), nil
	})

a := &agent.Agent{
	Instruction: "You are a helpful assistant.",
	Model:       anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: anthropic.ModelClaudeSonnet5_5}),
	Tools:       []tool.Tool{weather},
}
result, err := a.Run(ctx, agent.Text("What's the weather in Paris?"))
if err != nil {
	log.Fatal(err)
}
fmt.Println(result.Final.TextContent())
```

The full program is in [examples/quickstart](examples/quickstart).

## Core concepts

| Package | Responsibility |
|---|---|
| `chat` | Model protocol: messages, stream events, errors, structured output, model middleware; providers live in `chat/provider/*` |
| `tool` | Tool interface, execution context, results, typed tools, toolsets |
| `agent` | The Agent definition and its loop; sub-agents as a tool |
| `session` | Sessions and messages, the store interface, an in-memory store |
| `runner` | Runtime: loads history per session, drives the Agent, persistence, queues, compaction, background tasks, events |

Define tools → put them in an `agent.Agent` → hand the Agent and a store to a `runner.Runner` → call `Start` with a session ID and follow the run's events. Without sessions, call `Agent.Run` directly. See [docs/architecture.md](docs/architecture.md) for the design.

## Packages

| Package | Description |
|---|---|
| [`chat/provider/anthropic`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/provider/anthropic) | Anthropic Messages API and compatible endpoints |
| [`chat/provider/openai`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/provider/openai) | OpenAI Responses API and compatible endpoints |
| [`chat/provider/gemini`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/provider/gemini) | Google Gemini |
| [`chat/provider/deepseek`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/provider/deepseek) | DeepSeek |
| [`chat/imageproc`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/imageproc) | Normalizing images (format, size) before they enter history |
| [`tool/skill`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/skill) | Skills: instructions and resources loaded on demand |
| [`tool/todo`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/todo) | A checklist the model keeps for multi-step tasks |
| [`tool/shell`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/shell) | Shell commands, in the foreground or background, through a pluggable executor |
| [`tool/task`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/task) | Listing and stopping background tasks |
| [`tool/mcp`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/mcp) | Tools of MCP servers, over stdio or Streamable HTTP |
| `x/...` | Experimental packages, not covered by any stability promise |

## Examples

| Example | Shows |
|---|---|
| [quickstart](examples/quickstart) | An Agent with one typed tool |
| [runner](examples/runner) | Sessions that remember earlier turns, with streamed replies |
| [subagent](examples/subagent) | Sub-agents working in the background, results combined by the main Agent |
| [mcp](examples/mcp) | Using the tools of an MCP server over stdio |
| [approval](examples/approval) | A middleware asking a human before shell commands run |

Run them with `ANTHROPIC_API_KEY=... go run ./examples/<name>`.

## Out of scope

rein runs agents; the product around them is yours. It does not include:

- following runs across processes, or resuming them after a restart;
- permission and approval policies (middleware gives you the hooks);
- sandboxes for executing commands (implement `shell.Executor`);
- production session stores (implement `session.Store`; only an in-memory store ships);
- MCP resources, prompts, sampling and elicitation.

## Acknowledgements

The design of rein draws on [pi](https://github.com/earendil-works/pi), the agent harness by Earendil Works.

## License

[MIT](LICENSE)
