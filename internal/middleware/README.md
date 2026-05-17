# internal/middleware/

The Phase 4 NLP middleware. Translates `user.intent` envelopes into typed
tool calls via Gemini (or a deterministic mock) and dispatches them through
the orchestrator's existing chunk/result envelope flow.

## Public surface

```go
type Translator interface {
    Stream(ctx, TurnInput) (<-chan AssistantEvent, ResumeFunc, error)
}

type ToolCall struct {
    ID   string
    Tool string         // "execute_script" | "read_file" | "list_dir" | "write_patch"
    Args map[string]any
}

type Service struct {
    Translator Translator
    Dispatcher ToolDispatcher
    Approver   Approver
    History    history.Store
    Config     RuntimeConfig
}
```

## Translators

- `MockTranslator` — deterministic, dependency-free, default build. Used in
  tests and in the smoke flow when `NOMADDEV_MIDDLEWARE_RUNTIME=mock`.
- `GeminiTranslator` — real Google GenAI SDK client. Behind
  `//go:build gemini`. The default binary doesn't link the SDK; rebuild
  with `make build-gemini` to enable it.

`NewService(ctx, FactoryConfig{Runtime: "mock"|"gemini"|"none"})` picks one.
`Runtime: "none"` returns `nil`, which the orchestrator handler treats as
"reply with `event.error{not_implemented}`".

## Dispatcher

`CompositeDispatcher` routes by tool name:

- `execute_script` → `sandbox.Runner` (Phase 3 container).
- `read_file`, `list_dir`, `write_patch` → `internal/fsops.Engine` running
  as native Go on the workspace directory.

The split keeps `internal/sandbox` untouched and avoids the latency of a
container spin-up for filesystem-only ops. See `docs/middleware.md` for the
rationale and threat-model details.

## Approval gate

`PolicyApprover` decides whether a tool call needs a human round-trip
(`tool.approval.request` → `tool.approval.granted | denied`) before
dispatching. Default policy: `execute_script` and `write_patch` require
approval; `read_file` and `list_dir` auto-approve. See `docs/approval.md`
for the state machine and knobs.

## Persistent history

Per-session conversation turns are written to `internal/history.Store`
(SQLite by default). The translator's `Stream` receives a `TurnInput.History`
slice; the handler appends user / assistant / tool_call / tool_result turns
as the turn progresses.

## Running the Gemini tests

```sh
NOMADDEV_GEMINI_API_KEY=$KEY make test-gemini
```

The tagged suite skips cleanly when the env var is absent — CI never sets it.
