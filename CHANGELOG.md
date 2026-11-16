# Changelog

Breaking changes and notable additions per release. Before v1 any release may break the API.

## Unreleased

### Added

- `chat.ReasoningMax`, the highest effort: `max` on the Anthropic protocol (models without it set `Compat.NoMaxEffort`), `xhigh` on OpenAI's.
- `anthropic.Endpoint.Auth`: `AuthBearer` sends the key as `Authorization: Bearer` for endpoints that take it that way.
- `gemini.Config.MaxRetries`, as on the other protocols; 0 keeps the SDK's default of not retrying.
- `session.FileStore`: sessions as JSONL files in a directory, for local development and single-process use.
- `history.Tool(store)` (package `tool/history`): lets the model search and read the messages compaction replaced with a summary.
- A summary message records its compaction in `Message.Compaction`: the trigger, the estimated context before it and the tokens spent on the summary.
- `session.Compacted` returns the messages the latest compaction replaced; `Message.Transcript` renders a message as plain text.
- `storetest.Run` checks that messages round-trip with every field replay relies on.

### Breaking

- **Compaction metadata.** `session.Message.FirstKeptID` moved to `Message.Compaction.FirstKeptID`, `session.SummaryMessage` takes a `session.Compaction`, and `runner.CompactReason` with its constants became `session.CompactionReason` (`CompactionAuto`, `CompactionOverflow`, `CompactionManual`). Stored summaries written with `first_kept_id` are not migrated.

- **Models are described by the caller.** A client talks to one model, set as a value on `Config.Model`, and the built-in table of Claude models is gone. Only `Name` is required: a model is taken to speak the whole protocol, and `Compat` lists what it lacks. `Compat` is the same type on `Endpoint` (every model of the service) and on `Model` (that model only); a client lacks whatever either lacks.

  | Before | After |
  |---|---|
  | `Config{Model: "name"}` | `Config{Model: Model{Name: "name"}}` |
  | `Config.MaxInputTokens` / `MaxOutputTokens`, `Endpoint.MaxInputTokens` / `MaxOutputTokens` | `Model.MaxInputTokens` / `MaxOutputTokens` |
  | `anthropic.ModelClaude*`, `openai.ModelGPT6Luna`, `gemini.ModelGemini*` | removed; write the name |
  | openai `Config{Reasoning: true}` | the default; models that do not reason set `Compat.NoReasoning` |
  | gemini 2.x detected by name | `Compat{NoThinkingLevel: true, NoMediaInToolResults: true}` |
  | gemini `Config{BaseURL: u, Vertex: true}` | `Config{Endpoint: gemini.Endpoint{BaseURL: u, Vertex: true}}`, like the other protocols |

  What the Anthropic client used to infer from the model name is now declared. Models that were configured by name need these to behave as before:

  | Model | `Compat` |
  |---|---|
  | `claude-opus-5-5`, `claude-sonnet-5-5`, `claude-fable-*` | `NoDisableThinking` |
  | `claude-*-4-6`, `claude-*-4-7` | `NoThinkingBinding`, and `NoXHigh` for 4.6 |
  | `claude-*-4-5` | `NoAdaptiveThinking` |
  | `claude-3-*` | `NoAdaptiveThinking`, `NoToolSearch` |
  | other names (compatible endpoints) | `NoAdaptiveThinking`, `NoStructuredOutput`, `NoToolSearch` |

  Limits are no longer guessed either: without `MaxInputTokens` the runner does not compact history, and without `MaxOutputTokens` the Anthropic client sends `anthropic.DefaultMaxTokens` (32000) and the OpenAI client sends no limit.
- **`chat.Options.Model` is removed.** A request cannot pick another model; build another client and pass it as `agent.Input.Model` or `runner.WithModel`.
- **No vendor presets.** `anthropic.DeepSeek`, `openai.DeepSeek` and the `ModelDeepSeek*` constants are removed. Fill in an `Endpoint` yourself; the README lists the values for DeepSeek.

## v0.2.3 (2026-11-11)

### Breaking

- **Modules.** The model protocols, `tool/mcp` and `chat/imageproc` are modules of their own. Besides the core, `go get` each one you import, for example `github.com/coldcoder126/rein/chat/api/anthropic`.
- **`chat/provider` is now `chat/api`**, one package per protocol. Replace the import path.
- **Endpoints.** A vendor is an `Endpoint` of the protocol it speaks, set on `Config.Endpoint`:

  | Before | After |
  |---|---|
  | `Config{Provider: p, BaseURL: u}` | `Config{Endpoint: Endpoint{Provider: p, BaseURL: u}}` |
  | `deepseek.NewAnthropic(cfg)` | `anthropic.New` with `Endpoint: anthropic.DeepSeek` |
  | `deepseek.NewResponses(cfg)` | `openai.New` with `Endpoint: openai.DeepSeek` |
  | anthropic `TextOnly` / `BudgetThinking` / `UnsignedThinking` | `Endpoint.Compat.NoMedia` / `NoAdaptiveThinking` / `NoThinkingSignature` |
  | anthropic `PrefixMismatch: PrefixMismatchOff` | `Endpoint.Compat.NoThinkingBinding` |
  | openai `TextOnly` / `DisableStoreParams` / `DisableToolSearch` / `NoStructuredOutput` | `Endpoint.Compat.NoMedia` / `NoStoreParams` / `NoToolSearch` / `NoStructuredOutput` |

  The `deepseek` package is removed. `ModelInfo.Provider` and `ModelInfo.API` keep their values, so stored sessions replay as before.
- **Constructors return errors.** `anthropic.New`, `openai.New`, `gemini.New` and `mcp.New` return `(*Client, error)` instead of panicking.
- **Run context persists by default.** `runner.WithContext` stores its context in the session unless `Scope: session.ScopeRun` is set.
- A `runner.Compactor` gets `CompactInput.Messages` shared with the run and must not modify them.

### Added

- `session.TailLister`, an optional store interface for loading a session from its latest summary, and `session/storetest`, conformance checks for store implementations.
- A run reads the store once instead of before every turn.
- Deferred tools through the providers' server-side tool search (`chat.ToolDef.Deferred`).
- Claude 5 thinking blocks whose conversation changed are dropped by the API instead of failing the request (`anthropic.Config.PrefixMismatch`).
