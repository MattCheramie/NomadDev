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
                        // | "apply_code_patch" | "search_syntax" | "github_*"
    Args map[string]any
}

type Service struct {
    Translator              Translator
    Dispatcher              ToolDispatcher
    Approver                Approver
    History                 history.Store
    Tools                   []ToolSpec
    IsDestructiveGitHubTool func(name string) bool // classifies github_* mutators for audit mode
    Config                  RuntimeConfig
}
```

Use `Service.AvailableToolsFor(mode)` to fetch the per-turn catalogue
filtered for the request's mode (audit strips mutators);
`Service.IsMutatingTool(name)` reports whether a single tool mutates
host or remote state.

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

- `execute_script`, `search_syntax` → `sandbox.Runner` (Phase 3 container).
  `search_syntax` shells out to `sg` (ast-grep) inside the same ephemeral
  container; the worker image must carry the binary — see the `sandbox`
  Dockerfile target.
- `read_file`, `list_dir`, `write_patch`, `apply_code_patch` →
  `internal/fsops.Engine` running as native Go on the workspace directory.
- `github_*` → `internal/githubmcp.Caller` (subprocess MCP).

The split keeps `internal/sandbox` to the ops that genuinely need
container isolation and avoids container-spinup latency for filesystem-
only ops. See `docs/middleware.md` for the rationale and threat-model
details.

## Approval gate

`PolicyApprover` decides whether a tool call needs a human round-trip
(`tool.approval.request` → `tool.approval.granted | denied`) before
dispatching. Default policy: `execute_script`, `write_patch`, and
`apply_code_patch` require approval; `read_file`, `list_dir`, and
`search_syntax` are read-only and auto-approve. See `docs/approval.md`
for the state machine and knobs.

Audit mode (`user.intent.mode == "audit"`) is an orthogonal, stronger
restriction: mutating tools are *stripped* from the catalogue before
the schema reaches Gemini and refused at dispatch defense-in-depth —
not merely gated. See the "Audit mode" subsection of
[`docs/middleware.md`](../../docs/middleware.md).

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
