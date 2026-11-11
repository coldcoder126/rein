# Changelog

Breaking changes and notable additions per release. Before v1 any release may break the API.

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
