# Examples

Each example is a self-contained `main` package. None needs an API key: models are
`ScriptedProvider` or `ProviderFunc` fakes. Replace them with any `agentruntime.Provider`
to use a real model. Example 06 shows how to do that without long-lived secrets.

Run from the repository root:

```
go run ./examples/01-basic-chat
```

| # | Directory | Shows |
|---|---|---|
| 01 | `01-basic-chat` | Prompt side + human side, `{{var}}` interpolation from thread arguments, `SendMessage`/`WaitIdle`, transcript, run status |
| 02 | `02-function-tools` | Function tools with JSON Schema args, sequential execution, error results persisted (loop continues), per-thread KV from a tool |
| 03 | `03-subagents` | `subagent_create`, `RequiredSchema` argument validation, `SessionStop` binding, implicit result delivery that wakes the parent, child registry |
| 04 | `04-handler-hooks-effects` | Handler-backed side (no model), `before_create_message` hook, scheduled effect that queues a message, `EventSink` |
| 05 | `05-durable-filestore` | `FileStore` across a runtime restart, encrypted secret env, recovered KV, conversation continued by a fresh runtime |
| 06 | `06-oidc-workload-identity` | Real-model `Provider` authenticated by OIDC workload identity federation: projected or GitHub Actions OIDC token exchanged for a cached short-lived access token (RFC 8693 or Entra client assertion), HTTPS enforced. No API keys |
