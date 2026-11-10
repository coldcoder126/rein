# rein

[![CI](https://github.com/coldcoder126/rein/actions/workflows/ci.yml/badge.svg)](https://github.com/coldcoder126/rein/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/coldcoder126/rein.svg)](https://pkg.go.dev/github.com/coldcoder126/rein)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

[English](README.md) | 简体中文

rein 是一个用 Go 构建 LLM Agent 的框架，提供 Agent 循环、工具、会话以及围绕它们的运行时，优先支持 Anthropic 模型。

> **状态：** v0.x，v1 之前 API 可能有不兼容的变化。需要 Go 1.26 及以上。

## 特性

- **多家模型**：以 Anthropic 为主，另支持 OpenAI、Gemini、DeepSeek。消息、流式事件、错误分类、结构化输出和多媒体输入（内嵌、URL 或已上传的文件）都走统一的接口。
- **Agent 循环**：工具并发执行，带超时和结果截断；运行中可以插入引导消息和后续消息；中间件链可以包装运行、轮次、模型调用和工具调用。
- **工具**：用参数结构体定义带类型的工具，schema 自动生成；工具集可以按会话提供不同的工具；工具可以延迟加载，由模型通过 provider 的工具搜索按需找到（Anthropic、OpenAI），不破坏提示词缓存。
- **子 Agent 和后台任务**：把工作交给在后台运行的子 Agent；shell 命令也可以在后台运行。
- **Runner 运行时**：通过存储接口持久化会话；运行在后台进行，可以被任意多个视图订阅、随时重新接入；支持消息排队、自动压缩上下文，每次运行可以选用不同的模型。
- **内置工具**：技能（skill）、待办清单、shell、后台任务管理、MCP 服务器。

## 安装

```sh
go get github.com/coldcoder126/rein
```

## 快速开始

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

完整程序见 [examples/quickstart](examples/quickstart)。

## 核心概念

| 包 | 职责 |
|---|---|
| `chat` | 模型协议：消息、流式事件、错误、结构化输出、模型中间件；各家模型的实现在 `chat/provider/*` |
| `tool` | 工具接口、执行上下文、结果、带类型的工具、工具集 |
| `agent` | Agent 定义及其循环；子 Agent 作为工具 |
| `session` | 会话和消息、存储接口、内存存储 |
| `runner` | 运行时：按会话加载历史、驱动 Agent、持久化、排队、压缩、后台任务、事件 |

组合方式：定义工具 → 放进 `agent.Agent` → 把 Agent 和存储交给 `runner.Runner` → 用会话 ID 调用 `Start`，再订阅这次运行的事件。不需要会话时，直接调用 `Agent.Run`。设计细节见 [docs/architecture.md](docs/architecture.md)（英文）。

## 包一览

| 包 | 说明 |
|---|---|
| [`chat/provider/anthropic`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/provider/anthropic) | Anthropic Messages API 及兼容的接口 |
| [`chat/provider/openai`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/provider/openai) | OpenAI Responses API 及兼容的接口 |
| [`chat/provider/gemini`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/provider/gemini) | Google Gemini |
| [`chat/provider/deepseek`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/provider/deepseek) | DeepSeek |
| [`chat/imageproc`](https://pkg.go.dev/github.com/coldcoder126/rein/chat/imageproc) | 图片进入历史前的规整（格式、尺寸） |
| [`tool/skill`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/skill) | 技能：按需加载的说明和资源文件 |
| [`tool/todo`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/todo) | 模型处理多步任务时维护的待办清单 |
| [`tool/shell`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/shell) | 执行 shell 命令，可前台也可后台，执行环境可替换 |
| [`tool/task`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/task) | 查看和停止后台任务 |
| [`tool/mcp`](https://pkg.go.dev/github.com/coldcoder126/rein/tool/mcp) | 通过 stdio 或 Streamable HTTP 使用 MCP 服务器的工具 |
| `x/...` | 实验性的包，不做稳定性承诺 |

## 示例

| 示例 | 演示内容 |
|---|---|
| [quickstart](examples/quickstart) | 带一个类型化工具的 Agent |
| [runner](examples/runner) | 记得前面对话的会话，回复流式输出 |
| [subagent](examples/subagent) | 子 Agent 在后台工作，由主 Agent 汇总结果 |
| [mcp](examples/mcp) | 通过 stdio 使用 MCP 服务器的工具 |
| [approval](examples/approval) | 用中间件在执行 shell 命令前询问用户 |

运行方式：`ANTHROPIC_API_KEY=... go run ./examples/<name>`。

## 不在范围内的事

rein 负责运行 Agent，外围的产品由你来做。下面这些不包含在内：

- 跨进程订阅运行，或进程重启后恢复运行；
- 权限和审批策略（中间件提供了接入点）；
- 执行命令的沙箱（实现 `shell.Executor`）；
- 生产环境的会话存储（实现 `session.Store`；框架只自带内存存储）；
- MCP 的 resources、prompts、sampling 和 elicitation。

## 致谢

rein 的设计参考了 Earendil Works 的 Agent 框架 [pi](https://github.com/earendil-works/pi)。

## License

[MIT](LICENSE)
