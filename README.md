# rein

[![CI](https://github.com/coldcoder126/rein/actions/workflows/ci.yml/badge.svg)](https://github.com/coldcoder126/rein/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/coldcoder126/rein.svg)](https://pkg.go.dev/github.com/coldcoder126/rein)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

English | [简体中文](README.zh-CN.md)

rein is a Go framework for building LLM agents: the agent loop, tools, sessions and the runtime around them, with Anthropic models as the first-class target.

> **Status:** v0.x. The API may change in incompatible ways before v1. Requires Go 1.26+.

## Features

- **Model providers**: Anthropic first, plus OpenAI, Gemini and any endpoint compatible with their APIs behind one interface: messages, streaming events, classified errors, structured output, and media input inline, by URL or as uploaded files.
- **Agent loop**: parallel tool execution with timeouts and result truncation, steering and follow-up messages during a run, and a middleware chain wrapping runs, turns, model calls and tool calls.
- **Tools**: typed tools from a parameter struct, with the schema generated for you; toolsets that provide tools per session; deferred tools found through the provider's tool search (Anthropic, OpenAI) without breaking the prompt cache.
- **Sub-agents and background tasks**: delegate work to sub-agents running in the background; run shell commands in the background too.
- **Runner**: sessions persisted through a store interface, background runs that any number of views can follow and rejoin, message queues, automatic context compaction, and the model chosen per run.
- **Built-in tools**: skills, a checklist, shell, background task management, and MCP servers.

## Install

```sh
go get github.com/coldcoder126/rein
go get github.com/coldcoder126/rein/chat/api/anthropic
```

The core module depends on nothing but JSON Schema handling. Each model protocol (`chat/api/anthropic`, `chat/api/openai`, `chat/api/gemini`), `tool/mcp` and `chat/imageproc` is a module of its own with the same version, so you only pull the SDKs you use.

## Quick start

```go
type weatherArgs struct {
	City string `json:"city" jsonschema:"description=City name"`
}

weather := tool.New("get_weather", "Get the current weather of a city.",
	func(ctx *tool.Context, in weatherArgs) (*tool.Result, error) {
		return tool.Text(in.City + ": sunny, 22°C"), nil
	})

model, err := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: anthropic.Model{Name: "claude-sonnet-5-5"}})
if err != nil {
	log.Fatal(err)
}
a := &agent.Agent{
	Instruction: "You are a helpful assistant.",
	Model:       model,
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
| `chat` | Model protocol: messages, stream events, errors, structured output, model middleware; one package per protocol in `chat/api/*` |
| `tool` | Tool interface, execution context, results, typed tools, toolsets |
| `agent` | The Agent definition and its loop; sub-agents as a tool |
| `session` | Sessions and messages, the store interface, an in-memory store |
| `runner` | Runtime: loads history per session, drives the Agent, persistence, queues, compaction, background tasks, events |

Define tools → put them in an `agent.Agent` → hand the Agent and a store to a `runner.Runner` → call `Start` with a session ID and follow the run's events. Without sessions, call `Agent.Run` directly. See [docs/architecture.md](docs/architecture.md) for the design.

## Packages

| Package | Description |
|---|---|
| [`chat/api/anthropic`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/api/anthropic) | Anthropic Messages API and compatible endpoints |
| [`chat/api/openai`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/api/openai) | OpenAI Responses API and compatible endpoints |
| [`chat/api/gemini`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/api/gemini) | Google Gemini |
| [`chat/imageproc`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/imageproc) | Normalizing images (format, size) before they enter history |
| [`tool/skill`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/skill) | Skills: instructions and resources loaded on demand |
| [`tool/todo`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/todo) | A checklist the model keeps for multi-step tasks |
| [`tool/shell`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/shell) | Shell commands, in the foreground or background, through a pluggable executor |
| [`tool/task`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/task) | Listing and stopping background tasks |
| [`tool/history`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/history) | Looking up the messages compaction replaced with a summary |
| [`tool/mcp`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/mcp) | Tools of MCP servers, over stdio or Streamable HTTP |
| `x/...` | Experimental packages, not covered by any stability promise |

## Models and compatible endpoints

A client talks to one model, described by `Config.Model`; another model is another client. Only `Name` is required: a model is taken to speak the whole protocol, and `Compat` lists what it lacks. rein has no table of models, so a new model needs no new release. Declare `MaxInputTokens` to let the runner compact history, and `MaxOutputTokens` to set the default output limit.

```go
anthropic.Model{
	Name:            "claude-sonnet-5-5",
	MaxInputTokens:  1_000_000,
	MaxOutputTokens: 128_000,
	Compat:          anthropic.Compat{NoDisableThinking: true}, // thinking cannot be turned off
}
```

Another vendor speaking the Anthropic or OpenAI protocol is a `Config.Endpoint`: its address, the vendor name, how the key is sent (`Auth`), and a `Compat` of its own for what every model of the service lacks. A client lacks whatever its endpoint or its model lacks. rein ships no vendor presets. For DeepSeek, for example (as tested; check the vendor's current docs):

| Protocol | `BaseURL` | Limits (input / output) | Endpoint `Compat` | Model `Compat` |
|---|---|---|---|---|
| `anthropic` | `https://api.deepseek.com/anthropic` | 256,000 / 384,000 | `NoThinkingSignature` | `NoAdaptiveThinking`, `NoStructuredOutput`, `NoToolSearch` |
| `openai` | `https://api.deepseek.com` | 1,000,000 / 384,000 | `NoStoreParams`, `NoStructuredOutput`, `NoToolSearch` | |

Set `Provider: "deepseek"` on either. Add `NoMedia` for models that take no images, and on the OpenAI protocol `NoReasoning` for models that do not reason.

Fields a service adds outside the protocol pass through on the Anthropic and OpenAI clients: `Config.ExtraBody` adds top-level fields to every request (DeepSeek's `safety_check` or `title`; one named like a field the client builds replaces it), and `Config.ResponseFields` names the top-level response fields to keep, as raw JSON, in `Message.Extra` (DeepSeek's `title`).

Two things to know: a gateway in front of the protocol's own service is an `Endpoint` with just a `BaseURL`, and needs `Config.FileScope` set to keep using uploaded files, as a custom address reads no file store by default; and a service may ignore what it does not support rather than reject it, as DeepSeek does with a required tool call on the Anthropic protocol (per its docs).

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
- production session stores (implement `session.Store`; an in-memory store and a local JSONL file store ship);
- MCP resources, prompts, sampling and elicitation.

## Acknowledgements

The design of rein draws on [pi](https://github.com/earendil-works/pi), the agent harness by Earendil Works.

## License

[MIT](LICENSE)
