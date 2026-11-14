# go-agent-runtime

A Go implementation of the runtime layer of the
[Standard Agent Specification v0.2.0 — Runtime](https://standardagentspec.org/0.2.0/infrastructure/runtime/).

Package `agentruntime` executes dual-sided agents (human / prompt / handler backings) on
threads: it assembles model context, calls providers, runs tools sequentially, enforces
stop conditions, persists everything before advancing, and coordinates parent/child
subagent threads.

```go
reg := agentruntime.NewRegistry()
reg.AddProvider("openai", myProvider)               // implements agentruntime.Provider
reg.AddModel(agentruntime.ModelDefinition{Name: "fast", Provider: "openai", Model: "gpt-x"})
reg.AddPrompt(agentruntime.PromptDefinition{Name: "chat", Model: "fast", System: "You help {{user}}."})
reg.AddAgent(agentruntime.AgentDefinition{
    Name:  "assistant",
    SideA: agentruntime.SideConfig{Prompt: "chat"},
    SideB: agentruntime.SideConfig{Human: true},
})

store, _ := agentruntime.OpenFileStore("./data", key32) // durable; AES-GCM for secret/scoped env
rt := agentruntime.New(reg, store, agentruntime.Config{})
_ = rt.Start(ctx)                                        // recovers lost runs + durable queues
defer rt.Close()

th, _ := rt.CreateThread(ctx, agentruntime.CreateThreadOptions{AgentID: "assistant"})
_ = rt.SendMessage(ctx, th.ThreadID(), agentruntime.MessageInput{Role: agentruntime.RoleUser, Content: "hi"})
```

## Conformance map

Section numbers refer to the [Runtime spec](https://standardagentspec.org/0.2.0/infrastructure/runtime/).

| Section | Requirement | Where |
|---|---|---|
| 1 | One step at a time per thread; persist before continuing; limits; queued work | `runtime.go` (`schedule`/`runLoop`), `execution.go` |
| 1.1 | `ThreadState.Decide()` available at rest and during execution; never touches messages | `decide.go` |
| 2 | Triggers: user message, `queueMessage`, `queueTool`, scheduled effect | `SendMessage`, `QueueMessage`, `QueueTool`, `effects.go` |
| 3 | Step cycle order (drain → assemble → call → persist → tools → persist → stop); handler = one invocation; human parks | `promptTurn`, `handlerTurn`, `drive` |
| 4 | Context assembly order; stable snapshot; `filter_messages` once; inject never restarts; silent eligible; perspective mapping; vision filtering at request time | `context.go` |
| 5 | Sequential tools; errors persisted, never crash; function/prompt/handoff/lifecycle tool classes | `tools.go`, `subagents.go` |
| 6 | Stop order: sessionStop/Fail → stopTool → stopOnResponse → maxSteps / maxSessionTurns / hard cap | `promptTurn`, `drive` |
| 7.1–7.2 | Parent linkage, registry keyed by relationship, lifecycle tools, `maxInstances` actionable errors, implicit/explicit parent communication | `subagents.go`, `finishSession` |
| 7.3 | optional / immediate (recursive, object form) / hidden | `relationshipEnabled`, `spawnImmediate`, `lifecycleTools` |
| 7.3.1 | Parent-hook composition (child before → prompt/agent → child after), fail closed | `hooks.go`, `validate.go` |
| 7.4 | Argument validation vs child side-A `requiredSchema`, persisted, interpolated for both sides | `createChild`, `renderPrompt` |
| 7.5 | Scoped variable bootstrap `GET/POST /threads/{id}/variables/{rid}` | `subagents.go`, `bootstrap_http.go` |
| 8 | Attachment copying parent↔child; `notifyParent`, `setStatus`, `stopSession` | `copyAttachments`, `state.go` |
| 9 | Durable FIFO queue; inject before next model call when active; force a turn when idle; survives restart | `Store.Enqueue/PeekQueue/AckQueue`, `Start` |
| 10 | `terminate()` soft shutdown; session end never terminates a child | `terminate`, `finishSession` |
| 11 | No concurrent flows per thread | `threadCtl.running` |
| 12 | Status messages tagged `metadata.status_kind` | `queueToParent` |
| 14 (KV) | Durable per-thread KV, JSON round-trip, null deletes, isolation, documented caps | `state.go` (`GetValue`/`SetValue`) |

Run status (`idle`/`running`/`failed`, `since`, `reason`) follows Execution State §4,
including `execution_lost` recovery on `Start`.

### Intentionally not implemented (optional in the spec)

- **§13 Runtime Streams** — optional capability.
- **§14 Code Execution (`runCode`)** — required only when exposed; this runtime does not
  expose it.
- **§15 Thread Client API** — a SHOULD; not exposed (so its MUSTs do not apply). Hook up
  `Config.EventSink` to build one.

### Runtime-defined choices

- KV caps: key ≤ 512 bytes, value ≤ 1 MiB (JSON-encoded), ≤ 10 000 keys per thread
  (configurable via `Config`). Errors name the limit hit.
- Hard step cap: `Config.MaxTotalSteps` (default 100) → `max_total_steps`.
- Extra stop reasons: `execution_stopped` (`execution.stop()`), `effect_completed`.
- `setStatus()` writes the registry entry's `statusText` (working status); lifecycle
  transitions own `status`.
- Prompt includes use `{{>prompt_name}}`; variables use `{{key}}`.
- `FileStore` rewrites one JSON document per thread per mutation (fsync + rename), and a
  mutation becomes visible only after it is durable. It is a correctness reference; plug
  in your own `Store` for throughput.
- Crash semantics: a queued message is inserted and acknowledged atomically (no duplicate
  or lost delivery). A queued tool runs at most once; tool calls left without a result by
  a lost execution get an `interrupted` error result on `Start`. Effects are at-least-once
  (record deleted only after success; up to 3 attempts).
- Failed thread/subagent creation is rolled back, so no orphaned child threads remain.
- Required parent hooks from enabled relationships on **both** sides apply to every prompt
  (Hooks §1.3, Agents §6), ordered side A then side B, declaration order.

## Tests

```
go test ./... -race
```
