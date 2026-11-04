# Architecture

rein is an agent framework for Go. A model by itself is just a function from text to text; everything around it (the loop, tools, context, persistence) is provided by the packages below. They are top-level packages side by side in the repository root (built-in tools live in subpackages under `tool/`), and dependencies point one way:

| Package | Responsibility | Depends on |
|---|---|---|
| `chat` | Model protocol and providers: messages, stream events, errors, structured output, middleware | none |
| `tool` | Tool interface, execution context, results, typed tools, toolset interface | `chat` |
| `agent` | The Agent definition (instructions, model, tools) and its decision loop; sub-agents as a tool | `chat`, `tool` |
| `tool/skill`, `tool/todo`, `tool/shell`, `tool/task` | Built-in tools: skills, checklist, command execution, background task management | `tool` |
| `session` | Persistence model of sessions and messages, store interface, in-memory store | `chat` |
| `runner` | Runtime: loads history per session, drives the Agent, persistence, queues, compaction, events | `agent`, `session` |

Putting it together: define some tools and toolsets → put them in an `agent.Agent` → hand the Agent and a store to a `runner.Runner` → call `Start` with a session ID. Without sessions you can call `Agent.Run` directly.

## chat

`chat` can be used directly by other projects or packages, for plain chat, streaming chat or anything that only needs model access. It has no agent semantics.

Core boundaries:

- `Client.Stream`: the single entry point for model calls, returning `iter.Seq[Event]`; `Complete` / `Collect` consume the stream into the final message.
- `Message`: an assistant message's `Content` is an ordered list of blocks (text, thinking, tool calls) in the order the model returned them, restored unchanged on replay; it also records the provider, model, response ID and usage.
- `Options`: model parameters (Model, MaxTokens, Temperature and so on) are separate from messages and passed through by the agent unchanged. `Reasoning` (off / low / medium / high / xhigh) and `CacheRetention` (none / short / long) are provider-neutral and mapped by each provider to its own API.
- `Options.ToolChoice`: constrains tool calls (auto, none, required, a specific tool); ignored when the request has no tools.
- `Options.Output`: requires output that follows a JSON Schema; see "Structured output" below.
- `ToolDef`: only describes the schema of a tool the model can call, without execution logic; the schema is passed to the provider in full.
- `WithUsageRecorder`: records normalized usage at the `chat.Client` boundary; business attribution travels in `UsageContext` and `Options.Metadata`.

### Stream event protocol

Events start with `start` and end with exactly one `done` or `error`; text, thinking and tool call blocks come as paired start/delta/end events, and `Index` points to the block's position in the message; every event carries the message accumulated so far (a live object, only valid during the callback). Tool arguments are not streamed yet; `tool_call_end` gives the complete call.

Every provider produces events through `Builder`, which maintains the protocol invariants; `chattest.CheckStream` is the matching contract check, and provider tests run it against an httptest fake server.

### Error model

Once the stream is established nothing is thrown: every failure (including cancellation) becomes a terminal `error` event whose message keeps the content generated so far, with stop reason `error` or `aborted`; a tool call still open has incomplete arguments and is dropped. Errors are unified as a classified `*chat.Error` (rate limit, quota, server, network, auth, context overflow, refusal, canceled and so on), decided by HTTP status and headers; only context overflow and mid-stream errors need message matching. A dropped stream is a retryable network error.

### Structured output

- `Options.Output` gives a JSON Schema. When the model supports it natively (`ModelInfo.StructuredOutput`), each provider's constrained decoding is used: OpenAI's strict `json_schema`, Anthropic's `output_config.format`, Gemini's `responseJsonSchema`; for endpoints without it (such as DeepSeek) the provider writes the schema into the system prompt. Calling code stays the same.
- Results from both paths should be validated locally: `ParseOutput` strips code fences, parses the JSON and validates it against the schema; failures are `invalid_output`, and truncated output counts as a failure too.
- `CompleteJSON[T]` is the usual entry point: it generates the schema from a Go type, sends the request, validates and decodes. Schemas from `SchemaFor[T]` make every object field required and forbid extra fields, which satisfies every provider's strict mode; they also work for tool parameters.
- Partial JSON parsing during streaming is not supported yet.

### Middleware and hooks

- `Middleware` wraps a `Client` into a `Client`; `Chain` stacks several, the first being outermost. Logging, usage recording, retries and fallbacks all take this form.
- `Wrap` builds a middleware from a wrap function: it receives the request and `next`, and can rewrite the request, observe or rewrite events, or call `next` zero or several times.
- `WithHooks` is the simplified form for observing or slight adjustments: `BeforeRequest`, `OnEvent`, `AfterResponse`.
- `HTTPHooks` are debugging hooks at the raw HTTP layer (each provider's `Config.HTTPHooks`): `OnPayload` inspects or rewrites the request body actually sent, `OnResponse` reads the status code and headers.
- The `chat` layer only wraps model calls and knows nothing about runs or turns; wrapping that needs run information (runs, turns, model calls with turn info, tool execution) belongs to `agent` middleware.

### Media

- A media block is a content block carrying any content, such as images, PDFs, text files, audio and video: a MIME type plus either inline data or a URL, optionally with a file name. Media blocks can appear in user messages and tool results.
- `chat.Media` detects the type from the content, `chat.MediaURL` infers it from the extension; undetectable content is `application/octet-stream`, and `WithName` or `WithMIME` refine a type that is not specific enough.
- The framework only passes media through: it does not restrict types, download URLs, or convert or compress content. Whether a model accepts a type, whether a URL is reachable and when it expires, and whether the content fits size limits are the caller's responsibility; when it is not accepted the request fails and the error is returned as is. History is replayed every turn, so a rejected attachment or a dead URL makes every later request in the session fail; callers should check uploads when they receive them. Providers honor the target site's robots.txt when fetching URLs.
- Each provider maps media to its native input by MIME type: Anthropic supports images and PDFs inline and by URL, text files as plain text documents (inline only), and the Messages API has no input for other types, so they fail locally with `invalid_request`; OpenAI Responses uses `input_image` for images and `input_file` for everything else, leaving acceptance to the server; Gemini sends every type as inline data or a file URI.
- Uploaded files: a media block can refer to a file in a provider's file store (`FileRef`: scope, ID, expiry) instead of carrying the content, so long sessions do not resend it every turn. The Anthropic, OpenAI and Gemini clients have `UploadFile` / `DeleteFile`, built on the official SDKs; listing, quotas and clean-up stay with the caller. `ModelInfo.FileScope` names the store an endpoint reads (by default the provider name on the official endpoint, none on Vertex, compatible endpoints and custom base URLs; set `Config.FileScope` per workspace or project). On replay a file is used only when its scope matches the target and it is not about to expire; otherwise the block falls back to its URL or data, or becomes a note. So keep a fallback with `WithFile` when sessions may switch models or outlive the file (Gemini deletes files after 48 hours). File IDs are readable by every key of the store: keep them on the server and never accept them from end users.
- `ModelInfo.SupportsMedia` only says whether the endpoint accepts media input, regardless of type. When false (`TextOnly` endpoints) media blocks are replaced with a note before the request is sent.
- Media in tool results: Anthropic and OpenAI put it directly in the tool result; Gemini 3.x embeds inline data in `functionResponse`, while URLs and 2.x use separate parts right after it.
- Images generated by the model are media blocks too, delivered whole by an `image` event without deltas. `ImageOutput` declares whether generated images in history can be replayed to the model; otherwise they are replaced with placeholder text.
- Responses assistant messages cannot contain images, and a stateless `image_generation_call` refers to a server-side item by id and returns 404 on replay, so generated images in history are replayed as `input_image` in a user message after that turn's tool results.
- `chat/imageproc` is an optional image helper the framework never calls on its own: it detects formats from file headers, converts BMP and the like to PNG, scales oversized images down proportionally and picks the smaller of PNG and JPEG, applies EXIF orientation, and writes a size note for the model. `imageproc.Fit` processes an image for the model's `ImageLimits` and returns content blocks directly. Call it **once, before an image first enters history**: an oversized image makes the server reject every later request, and modifying history afterwards breaks the prompt cache.

### Replay normalization

`NormalizeForModel` runs before every provider request: it drops error/aborted assistant messages, fills in missing tool results and drops orphan results, downgrades thinking blocks to text and normalizes tool call IDs across models, and replaces media the model does not accept with a note.

Real providers live in `chat/provider/*`. Each requires `Config.Model` and has no built-in default, so upgrading rein never changes the model you run; the model constants are only shorthands.

- `anthropic`: the Anthropic Messages API.
- `openai`: the OpenAI Responses API, also for endpoints compatible with it; there is no Chat Completions implementation.
- `gemini`: built on the official `google.golang.org/genai` SDK; it uses the Gemini API by default, and the `Vertex` switch supports Vertex API keys. Full Vertex access with projects and credentials is not provided yet.
- `deepseek`: constructors for DeepSeek; the Anthropic endpoint reuses `anthropic` and the Responses endpoint reuses `openai`.

Responses is used statelessly (`store:false`), so replay relies on block signatures: thinking blocks keep the full reasoning item (with encrypted content once `Reasoning` is on for reasoning models), text blocks keep the message id and phase, and tool call blocks keep the item id of the function_call. These signatures are only valid for the original model and are cleared across models by `NormalizeForModel`. DeepSeek's chain of thought comes as plain reasoning items that are also sent back whole; it does not recognize `store` / `include`, so the presets do not send them.

Anthropic notes:

- Model specs are built in (context and output limits, thinking style), taken from the official model list API; `Config` can override them.
- Thinking: 4.6 and later models use adaptive thinking with `effort`; the 4.5 series and compatible endpoints use a thinking budget that shares `max_tokens` with the answer, so the limit is raised accordingly; models that cannot turn thinking off map `off` to the lowest effort; `temperature` is not sent with thinking on.
- Caching: by default cache breakpoints go on the system prompt and the last block of the last user message, covering tool definitions, the system prompt and the whole history; `long` uses a 1 hour TTL.
- Whether forced tool calls (required, a specific tool) work with thinking depends on the model: models that cannot turn thinking off (Sonnet 5.5, Opus 5.5, Fable) do not support them and fail directly; budget thinking models turn thinking off for that request; adaptive thinking models allow both.
- Unsigned thinking blocks are replayed as text (compatible endpoints can keep them with `UnsignedThinking`); when truncated output leaves tool arguments incomplete, the arguments are cleared and the reply ends with `length`, and upper layers should not execute that call.

Gemini notes:

- A `thoughtSignature` may appear on any part; it is stored as base64 in the signature of the corresponding content block and stays on its original part on replay. Gemini 3 requires signatures on function calls, and historical calls from other models use the official placeholder signature to skip validation.
- Thinking: 3.x uses `thinkingLevel` and 2.x a thinking budget, with `off` mapped to a budget of 0; thought summaries are requested by default.
- Function calls without an id get a locally generated one that is not sent on replay; consecutive tool results are merged into one message; for media in tool results see "Media" above.
- `ImageGeneration` asks for text plus image output, and generated images are replayed as is in assistant messages.
- In testing, `gemini-3.5-flash` on Vertex intermittently hangs on the server with forced tool calls while other models are fine; use a timeout when forcing calls.
- Caching is implicit on the server, so `CacheRetention` has no effect; safety blocks are refusals, and malformed function calls are retryable server errors.

Unit tests never call external model services; real API checks use an explicit integration switch (`REIN_INTEGRATION=1`). `chat/provider/anthropic`, `chat/provider/openai` and `chat/provider/gemini` each have a set, taking keys from environment variables or a local `env.yaml`. `agent` and `runner` only have unit tests that cover their behavior with fake models; compatibility with real APIs is guaranteed by the provider integration tests.

## tool

Writing a tool only needs the `tool` package.

- `Tool`: two methods, `Def` returning the definition the model sees and `Execute` running it. `tool.New[In]` creates a tool from a parameter struct: the schema is generated from the type (fields without `omitempty` are required) and arguments are decoded automatically.
- `Context`: what a tool receives when it runs. It is a `context.Context`, plus the run info and this call, with `Update` for reporting progress. The run info has the session ID, run ID, parent and root session IDs, the sub-agent nesting depth (0 for the main Agent), and the name of the running Agent (empty for a general sub-agent). Inside a sub-agent the session ID is the sub-task's own session, so tools that look up data by the user's conversation should use the root session ID.
- `Result`: `Content` enters the model context of the next turn; `Details` are for the UI, logs and business; `IsError` marks an error; `Terminate` ends the run when every result of the batch asks for it; `Usage` is the tokens consumed inside the tool, counted in the run total.
- Optional interfaces: `Sequential` requires the whole batch containing the tool to run sequentially, and `Timeouter` declares the tool's own timeout. With `tool.New` they are set through options.
- Background tasks: `TaskManager` is the background task manager the runtime (the runner) provides to a session; tools get it with `tool.TaskManagerFrom(ctx)`, and nil means background tasks are not supported. A tool hands it a `TaskSpec` (type, description, run function) to run in the background, and the runtime sends the result back to the session as a notice when it ends; snapshots are a common `Task`, with `Type` telling sub-agents, background commands and so on apart. Tools that depend on background tasks implement `NeedsTaskManager`, and the Agent hides them without a `TaskManager`.
- `Toolset`: a source of tools in bulk (skills, MCP servers). It is resolved once at the start of each run and can return different tools per session; the tool list stays fixed within a run so the prompt cache is not broken. A toolset can also implement `Instructor` to append instructions to the system prompt.

## agent

An `Agent` is a definition: name, description, instructions, model, model options, tools and toolsets, limits, and middlewares. It holds no session state, so the same Agent can run for many sessions concurrently. `Agent.Run` drives it to the end, taking the history so far as input.

- Instructions can be a fixed string or generated at the start of each run by `InstructionFunc`; instructions contributed by toolsets are appended.
- `Options` are `chat.Options`, passed unchanged to every model call with `turn_index` metadata added. Extensions at the model call level (logging, usage, retries) wrap `Model` with `chat.Chain`.
- `Model` is the default model and may be nil when every run passes one; `Input.Model` overrides it for one run, for products where users pick the model per session or per message. General sub-agents started by that run use it too, and specialized ones keep their own. `Options` stay the same across models, so leave model-specific options such as `Options.Model` and `MaxTokens` unset when switching.
- `agent.Text` and `agent.Parts` build an input with a single user message. `agent.RunJSON[T]` runs the Agent and decodes the final reply into `T`: without an output format the schema is generated from `T`, tools and middlewares work as usual, and the format only constrains the final reply; when the reply is invalid the validation error is given to the model and the Agent runs once more (truncated output is not retried). The format constraint is sent with every model call, and some weaker models (claude-haiku-4-5 in testing) may skip tools under the constraint and make up a result; use a stronger model then. Unlike `Run`, every case without a result (model failure, cancellation, turn limit, still invalid after the repair) returns an error, and `Result` is still valid.
- `agent.SubAgentTool` returns the `sub_agent` tool, letting the Agent hand sub-tasks to sub-agents in the background; the model decides whether to delegate and to whom. It only starts them: it returns the task ID (the child session ID) immediately, the result comes back to the main session as a notice when the sub-task ends, and the main Agent does not wait; several calls in one turn run in parallel. Listing and stopping use the `task` tool (see "Built-in tools"); provide both together.
- Sub-agents start cold: they only see the task description written by the main Agent and run to the end in a fresh context. The fallback limit is the turn limit (`MaxTurns`).
- The tool depends on background tasks, so only a main Agent run by the runner has it: it is not offered when `Agent.Run` is used directly or inside a sub-agent, so sub-agents cannot spawn sub-agents or other background tasks. Sub-tasks are registered in the runner's task table; the agent package keeps no state.
- The general sub-agent borrows the main Agent's model, options (except the output format), tools and limits, and uses its own instruction (configurable, a built-in neutral instruction by default); tools can be excluded by name (when the main Agent has no toolsets, a misspelled name makes the sub-task fail to start). It does not inherit the main Agent's middlewares: those are the main Agent's own behavior, and middlewares for every Agent belong to the runtime (see "Middleware"). Specialized sub-agents use their own definitions and are picked by name. The model cannot set a sub-agent's instruction or tools.
- Every sub-agent's system prompt ends with a notice: the result goes in the final reply, which is all the caller sees, and messages from the caller cannot stand in for the user's authorization.
- Inherited tools are the same instances as the main Agent's; sequential constraints only hold within one run, so a background sub-agent may call the same tool at the same time as the main Agent.

### Control flow

A run (one call to `Agent.Run`) is a two-level loop. One iteration of the inner loop is a turn: inject pending messages, call the model once, and execute the tool calls it produced; `TurnIndex` is numbered consecutively from 0 within a run, and `MaxTurns` limits the number of turns. The inner loop alternates between model calls and tool execution, taking messages from `Input.Steering` after each turn and injecting them before the next model call; when the inner loop has nothing left to do, the outer loop takes messages from `Input.FollowUp` to decide whether to continue. Steering never interrupts a batch of tools in progress. The Agent keeps no queues itself; the upper layer (the runner) provides both callbacks.

### End status

`Result.Status` gives the reason the run ended: `completed`, `max_turns`, `failed` or `aborted`. Every ending emits `agent_end`.

The error contract is the same for `Agent.Run`, `agent.RunJSON` and `runner.Run.Wait`: `error` is nil only on normal completion; a model failure returns a classified `*chat.Error`, cancellation returns the ctx error, and the turn limit returns `ErrMaxTurnsExceeded`, and in these cases `Result` is still valid.

- A failed assistant message is included in the result, and `Final` keeps the error and the content generated so far. `MaxTurns` defaults to 20.
- Invalid input, or a failing toolset, middleware or event handler, also returns an `error`, in which case `Result` may be nil.

### Events

Received through `Input.Emit`:

- Lifecycle: `agent_start`, `agent_end`, `turn_start`, `turn_end`
- Messages: `message_start`, `message_update`, `message_end`; every event carries a snapshot of the message, so any single event is enough to render the current state. Snapshots are safe to keep but read-only: they share the bytes of media data and tool arguments with the streaming message instead of copying them per event
- Tool execution: `tool_execution_start`, `tool_execution_update`, `tool_execution_end`

### Middleware

A `Middleware` is a set of wrapping hooks; one concern (tracing, auditing, human approval) is packaged as one. Middlewares form a chain of responsibility with the first being outermost. By ownership there are three layers, from outside in:

- Runtime: `runner.Config.Middlewares`, applied to every run on that Runner, including the sub-agents it spawns. Steps every Agent must go through, such as approval, auditing and tracing, go here, so no inheritance rules are needed.
- Agent: `Agent.Middlewares`, belonging to that Agent definition only and not inherited by sub-agents. An Agent can only add its own behavior inside the runtime's.
- Single run: `Input.Middlewares`, hooked in by the upper layer (the runner's history compaction). When `Agent.Run` is used directly, global middlewares are passed in here as well.

The four hooks nest from outside in, matching the structure of a run:

- `WrapRun`: wraps a run, covering the resolution of the prompt and tools and every turn.
- `WrapTurn`: wraps a turn; with the turn's result it can end the run (setting the status to `completed`), replace the history used by later turns (the entry point for compaction), or recover a failed turn so the loop calls the model again with the replaced history. The turn's messages were already emitted as events and cannot be changed.
- `WrapModel`: wraps a model call with access to the turn info; it can change messages, the model and tool definitions without writing back to history. Use `BeforeModel` when only the request changes.
- `WrapTool`: wraps a tool execution. Changing arguments, rewriting results, intercepting, human approval and timing all use it; a returned `error` becomes an error result for the model and does not stop the run.

Observing callbacks (`Input.Emit`, the runner's task callbacks) are not part of the chain.

### Tool execution

Pipeline: look up the tool → validate arguments against the schema → middleware `WrapTool` → execute → cap the result size. Every tool call is guaranteed a result:

- The tools callable in a turn are exactly those actually sent to the model after `WrapModel`; tools not offered are not executed even if called.
- A missing tool, invalid arguments, a tool error or a panic all become error results for the model to correct.
- When model output is truncated (`length`), none of the turn's tool calls run; they get a "truncated" error result and the model retries.
- After cancellation, calls that have not started get a "canceled" error result, and the run ends as `aborted`.
- Before returning, all in-flight tools are awaited, except those that timed out.
- A batch of tools runs concurrently by default; `tool_execution_end` is emitted in actual completion order, while tool result messages enter history in the model's original call order, keeping the transcript stable.

Limits (all with defaults):

- `ToolTimeout`: timeout of a single execution, none by default. After a timeout an error result is returned and the loop continues without waiting for the tool, dropping its later progress, so tools should honor cancellation.
- `MaxParallelTools`: concurrency within a batch, 8 by default.
- `MaxToolResultBytes`: text limit of a single result, 100KB by default; beyond it the start and end are kept with a note of the bytes omitted, and media and `Details` are not affected.

## Built-in tools

The ready-made tools live under `tool/`, one subpackage each, depending only on `tool`. Neither Agent nor Runner knows about them; they take effect once added to `Agent.Tools` or `Agent.Toolsets`, and can be replaced with your own implementations. The sub-agent tool is the exception: it reads loop internals, lives in the `agent` package and cannot be replaced.

Background capabilities are split into starting and managing: each starts with its own tool (`sub_agent`, the background mode of `shell`), while listing and stopping go through `task`, and completion notices have one form. When you use background capabilities, provide `task` too; the framework does not add it automatically.

### skill

Skills are focused instructions for the model, optionally with resource files. To save context, the system prompt only lists the skill catalog; the model loads full instructions when needed and reads resources as needed.

- `Source`: where skills come from, with three methods: list the catalog, load a skill (instructions plus resource list), read a resource file. Permissions, publishing state and scenario filters are up to the source implementation.
- Built-in sources: `skill.Dir` (local directory), `skill.FS` (`fs.FS`, compatible with `embed`), `skill.Static` (memory). In a directory source every directory containing a `SKILL.md` is a skill, and the other files in it are its resources. Database or remote sources just implement `Source`.
- `skill.NewToolset`: turns a source into a toolset for `Agent.Toolsets`. It appends the skill catalog to the system prompt and provides two tools, `load_skill` and `read_skill_resource`.
- Resources can only be read as text. Scripts bundled with a skill can be read but are not yet connected to `shell` (placing skill files in the execution environment).

### todo

`todo.Tool()` provides the checklist tool `todo_write`, letting the model break a multi-step task into a checklist and update it as it goes.

- Each call passes the complete list, replacing the previous one. The tool keeps no state: the current list is the result of the latest successful call in the session history, and sub-tasks have their own lists in their own sessions.
- Each item only has content and a status (pending, in progress, completed). Small problems are fixed rather than reported (whitespace, empty items, keeping only the first in-progress item).
- The result for the model is a single line of progress; the normalized list is in `Details`, which the UI takes from the tool execution end event.
- Using it well depends entirely on the tool description; reminders in the loop (such as when the list has not been updated for several turns, or restoring the list after compaction) are not implemented.

### shell

`shell.Tool(cfg)` provides the command tool `shell`. Where commands run is decided by an `Executor`, which must be passed explicitly; the framework only provides `shell.Local`, which runs on this machine, while containers and remote sandboxes are implemented by the business layer. Running commands hands the execution environment to the model, so production should isolate it; use `WrapTool` when approval is needed.

- Foreground: waits for the command to finish or time out (2 minutes by default, settable by the model up to 10 minutes by default) and returns the merged stdout/stderr and the exit code, reporting output live through `Update`. A non-zero exit code or a timeout is an error result. `Details` is `shell.Details`.
- Background (`run_in_background`): runs as a background task of type `shell` and returns the task ID immediately; there is no timeout, it runs until the command ends or is stopped. When it ends, the exit code and output come back to the session as a notice, and a non-zero exit code fails the task. Without background task support (using the Agent directly, inside a sub-agent) it fails and the model falls back to the foreground.
- Every call starts in a fixed working directory, without keeping the previous `cd` or environment variables. Only the start and end of the output are kept, so commands that flood output cannot fill memory and the context.
- `Local` runs each command in its own process group, ending child processes too on cancel or timeout; it is only suited to development or trusted environments.

### task

`task.Tool()` provides the background task tool `task`, managing every kind of background task spawned by the current session.

- `status`: without an ID lists the running tasks (without output); with an ID returns the full snapshot, including the tail of the output and type-specific `Details`.
- `stop`: stops a task and waits for it to end; the result still comes back to the session as a notice.
- When a task is not found, the error lists the running tasks so the model can correct itself.

### mcp

`mcp.New(name, transport, opts...)` connects to one MCP server and provides its tools as a toolset for `Agent.Toolsets`. It is built on the official Go SDK (`modelcontextprotocol/go-sdk`), which negotiates the protocol version: the stateless 2026-07-28 revision, falling back to the initialize handshake of older servers. Only tools are supported; resources, prompts, sampling, elicitation, roots and the tasks extension are not.

- Transports: `mcp.Stdio` (a local subprocess, started anew on every connection) and `mcp.HTTP` (Streamable HTTP; credentials go through the caller's `http.Client`). The OAuth flow is not implemented.
- Connection: made on the first run and checked at the start of every run, reconnecting when it has dropped; there is no reconnection in the middle of a run. One client is shared by all sessions and runs, and the business closes it. A server that cannot be reached does not fail the run: it contributes no tools and the system prompt says it is unavailable.
- Naming: `mcp__{server}__{tool}`, as in Claude Code, so middlewares can match a whole server by prefix. Characters model providers reject become `_`; a tool whose name is still too long or collides is skipped and reported in the system prompt. `WithFilter` narrows the tools.
- Tool list: cached until the server sends a list-changed notification or, on 2026-07-28 servers, its `ttlMs` expires; a failed refresh keeps the previous list. The server's instructions are appended to the system prompt unless `WithoutInstructions` is given.
- Calls: text becomes text blocks; images, audio and binary embedded resources become media blocks; resource links become a line of text; `structuredContent` goes to `Details` (and to text when there is no other content); `isError` carries over. Protocol errors, timeouts (`WithTimeout`, otherwise `Agent.ToolTimeout`) and requests for user input become error results for the model. Output size is capped by `Agent.MaxToolResultBytes` like any tool.
- Approval: tools are `*mcp.Tool`, exposing the server, the remote name and the declared annotations (read-only, destructive and so on) for `WrapTool` middlewares. Annotations are untrusted hints; the framework does not act on them.

## session

`session` only describes what is stored and where; a storage backend only needs to depend on it.

- `Message`: a session message, with its role (user, assistant, tool, business context, summary), UI visibility, source, business kind and lifetime. The content is stored once, in `ChatMessage`: assistant and tool messages are the raw messages produced by the model (thinking, tool calls, signatures, usage) and are replayed as is, while other roles only use its content blocks. `Text()` gives the display text and `Content()` the content blocks. `Source` and `Kind` belong entirely to the business layer; the framework does not depend on their values.
- `Session`: session metadata. A non-empty `ParentID` means the session belongs to a sub-task rather than a conversation the user created, and listings of the user's sessions should usually exclude it.
- `Store`: four methods (ensure a session exists, get a session, append messages, list messages); `MemoryStore` is the in-memory implementation.
- `DisplayMessages` filters the messages shown to the user.

## runner

A `Runner` holds one Agent and one store and serves any number of sessions: different sessions can run concurrently, and each session has at most one run at a time. That mutual exclusion only holds within one Runner instance; when several processes or instances share a store, the caller must ensure it. Who the Agent is is defined on `agent.Agent`, and the Runner only runs it; do not modify the Agent after handing it to the Runner.

### Runs

- `Config.Middlewares` are the runtime middlewares; see "Middleware" under agent. Sub-tasks of spawned sub-agents use the same set.
- `Start(ctx, sessionID, input)`: processes one user input, creating the session if needed, and returns a `*Run` as soon as the run has started; a busy session gets `ErrBusy` at once. One run drives exactly one Agent run to the end in the background: it keeps the values of `ctx` but not its cancellation, so it does not depend on anyone watching it. `Run.Wait` returns its result.
- `Steer`: interjects mid-run, injected after the current turn's tools finish and before the next model call.
- `FollowUp`: queues a follow-up task, injected when the model gives its final reply and the run would end.
- `Abort`: cancels the session's current run and returns the queued messages not yet injected. `Running` reports whether the session is occupied, and `Runner.Wait` waits for that to end, which only needs the session ID (for example to start again right after stopping).
- The input is an `Input`: content blocks that can only be text and media. Use `runner.Text` for plain text and `runner.Parts` with attachments. The content enters history as is; thinking and tool call blocks are rejected. An empty input (`Input{}`) appends nothing and continues from history, for waking the main Agent to handle background task results written to history; the history must end with a user-side message or a tool result.
- `WithModel` runs this run on another model than `Agent.Model` (see `agent.Input.Model`); compaction during the run follows it, for the context limit and for generating the summary. `Agent.Model` may then be nil; `Start` and `Compact` fail at once when no model is given. So one Runner serves every model, and stopping, queues and background tasks do not depend on which model a session uses.
- `WithContext` injects business context: by default it only enters this run and is not written to the session, which suits temporary information such as page state; with the persistent scope it is written to the session and stays visible to later runs. Source and kind follow the business layer's conventions, with no framework defaults.

`Result.Status` gives the end status, and `Pending` the queued messages not injected in time. When the model fails, the run is canceled or the turn limit is reached, `Run.Wait` also returns a non-nil `error`, and `Result` is still valid.

### Background tasks

Background tasks spawned by the main run (sub-agents, background commands and so on) are registered in one task table on the Runner, with `tool.Task.Type` telling them apart. The Runner injects the session's `tool.TaskManager` into every main run; sub-task runs do not get one, so sub-agents cannot spawn background tasks.

- Tasks do not follow the run that started them: neither the end of the main run nor `Abort` cancels them, and only the model's `task(stop)` or `StopTask` stops them. `ListTasks(sessionID)` lists the session's tasks whose results have not been delivered. Task snapshots keep only the last 32KB of output. Running tasks are lost when the process exits.
- Each sub-agent task runs in a new child session whose ID is the task ID: a Runner sharing the store runs the sub-agent, so storing, replay and compaction work as in the main session and every message can be audited. The task ID leads from parent to child, and the session metadata's `ParentID` from child to parent. A sub-task snapshot's `Details` is `agent.SubAgentDetails` (turns, latest tool, usage).
- When a task ends (completed, failed, stopped), its result comes back to the session that spawned it as a notice message: a `<task_result>` with the task ID, type, status and description around the result or error; its `Kind` is `task_result`, it is hidden from the user by default, and `Details` is the task's final snapshot.
  - While the session runs: the notice is injected between turns, and one arriving when the run would end makes it run one more turn; notices not injected in time are stored when the run ends.
  - While the session is idle: it is written directly to history (the session is briefly occupied meanwhile, and new runs get `ErrBusy`).
- Three callbacks track tasks of every kind, all called synchronously on the task's goroutine:
  - `OnTaskStarted`: after the task starts, called first.
  - `OnTaskProgressed`: every time the task reports progress, for live display. Sub-agent progress carries the run events of the child session, and background command progress carries the new output.
  - `OnTaskFinished`: after the result has been sent back to the session, called last. With `ResultInjectedIntoRun` the main Agent is already handling it; with `ResultSavedToHistory` the main Agent has not seen it yet, and the business layer decides whether to wake it with an empty input. The framework never wakes it on its own.
- The concrete type of `Details` after a store round trip depends on the store implementation (a JSON store reads it back as a map).

### When messages are stored

- The user message is stored before the run starts; assistant and tool messages are stored as each one ends.
- Steer / follow-up messages are stored when actually injected, and those canceled or not injected in time never enter history; background task result notices are the exception, and those not injected are still stored when the run ends.
- Every message carries the `RunID` of the run it was written in (a manual compaction counts as a run), so all messages of a run can be retrieved by it; background task results written directly while the session is idle belong to no run and have an empty `RunID`.
- Failed or canceled assistant messages are stored as usual and excluded by the chat layer when replayed to the model. Storing ignores the run's cancellation, so store implementations need not handle a canceled `ctx`.
- History left with tool calls but no results by an interrupted process is filled in by the chat layer on the next load, so it can simply continue.

### Compaction

Long sessions run into the model's context limit. The Runner decides when and how to compact, and the Agent only provides the entry point for replacing history mid-run: the Runner hooks compaction in as the `WrapTurn` of a run-level middleware.

- `Compactor` is the strategy interface: it takes the session messages currently in the context, the estimated usage and the model limit, and returns a summary plus the message where the kept part starts, or nil for no compaction. Implementing it replaces the strategy entirely; `DisableCompaction` turns it off.
- The default strategy `SummaryCompactor` (on by default) triggers above 80% of the limit, keeps the most recent messages worth about 20% of the limit as they are, and has the model summarize the earlier part. The cut prefers user messages and never separates a tool call from its result. The trigger ratio, keep ratio, summary prompt, summary model and summary request parameters are all configurable; by default the summary request outputs at most 4096 tokens and writes no prompt cache.
- It is called automatically at three points: before each run; after each turn with tool execution; and when a turn fails because the context was exceeded, it forces a compaction and recovers that turn within the same run, calling the model again with the compacted history, at most once per run. So one runner run always calls `Agent.Run` exactly once. `CompactInput.Reason` tells these cases and manual compaction apart.
- Manual compaction: the business layer calls `Runner.Compact` when the user asks for it, optionally with extra requirements for the summary and the model to summarize with (usually the session's current model), and gets the summary message and the tokens consumed generating it. It only works while the session is idle, and occupies the session meanwhile. The default strategy then ignores the threshold and cuts by the overflow rules; manual compaction also works when the model declares no context limit.
- Usage is estimated from the real usage of the latest assistant message after the summary, with the messages after it estimated roughly by bytes. There is no automatic compaction when the model declares no context limit.
- The summary is stored as a session message with the `summary` role, recording where the kept part starts; history is built as "latest summary + the messages it keeps + the messages after it". The original messages all stay in the store, so a UI can still show the full history.

### Events

Every event of a run is kept in memory with the `Run`, and `TextDelta()` / `ThinkingDelta()` on an event give the text and thinking deltas directly: `run_start`, `message_saved`, `queue_update`, `agent_event`, `compaction`, and finally `run_end` with the run result. `message_saved` is only emitted after the message is stored; every event is sent in order by the run itself, including queue changes caused by `Steer` or `Abort` from other goroutines.

- `Run.Events` replays the run's events from its start and then hands over new ones until `run_end`. Any number of receivers can iterate at their own pace, and none of them holds up the run.
- `Runner.Subscribe(ctx, sessionID)` follows a session rather than one run: it replays the run in progress, then continues with every later run until `ctx` ends, so a reopened page or a second tab can attach at any time, including to runs started elsewhere (a bot, a woken Agent). Earlier runs are read from the store.
- To bound memory, a replayed `message_update` keeps only its delta once a newer one exists; `message_end` carries the complete message.

Events live in the process that runs them: following a run from another instance, or after a restart, belongs to the business layer.

Not implemented yet: session trees, JSONL storage, session-level key-value state, and pausing and resuming across processes.

## Experimental x

Packages under `x/` are experimental and not yet connected to `agent` / `runner`. They are not stable API and may change or be removed at any time:

- `x/notify`: external notification channels such as Telegram and webhooks.
