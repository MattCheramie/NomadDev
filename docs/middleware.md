# NLP middleware (Phase 4)

Phase 4 plugs Gemini in front of the Phase 3 sandbox: a client sends a
`user.intent` envelope with free text, the middleware drives a turn against
the LLM, dispatches any tool calls the model emits, and streams back
`assistant.chunk` and `assistant.message` envelopes. Tool I/O reuses the
existing `command.request` / `command.chunk` / `command.result` shape so the
client renders all output the same way regardless of who originated the
call.

## Components

```
internal/event             new envelope types + payloads
internal/middleware        Translator interface + MockTranslator + GeminiTranslator
internal/middleware/...    ToolDispatcher (composite over sandbox + fsops)
internal/middleware/...    PolicyApprover (RequiresApproval/Register/Signal/Await)
internal/history           Store interface + MemoryStore + SQLiteStore
internal/fsops             Native filesystem ops for read_file/list_dir/write_patch
internal/wsserver/middleware.go   handleUserIntent + runIntent + runToolCall
```

`wsserver.New` takes a `*middleware.Service` bundle (`Translator`,
`ToolDispatcher`, `Approver`, `history.Store`, runtime knobs). A nil bundle
means `user.intent` returns `error{not_implemented}` — same posture as the
nil-runner contract from Phase 3.

## Translator

```go
type Translator interface {
    Stream(ctx, TurnInput) (<-chan AssistantEvent, ResumeFunc, error)
}
```

One `Stream` call corresponds to one `user.intent` turn. Each emitted
`AssistantEvent` is exactly one of:

- `Text`: a streamed text chunk → sent as `assistant.chunk`.
- `ToolCall`: a discrete function-call instruction. The translator stops
  emitting on the current channel; the handler dispatches the tool and
  calls `ResumeFunc(ctx, ToolResult)` with the result, which returns a
  fresh channel for the continuation.
- `FinalMessage`: terminal frame. The handler emits `assistant.message`
  and closes the turn.
- `Err`: fatal turn error.

### MockTranslator

Default-build deterministic stub. Tests load a `Script [][]AssistantEvent`
(stage 0 fires on `Stream`; stage N>0 on the Nth `Resume`). Used in CI and
in the smoke flow when `NOMADDEV_MIDDLEWARE_RUNTIME=mock`.

### GeminiTranslator

Behind `//go:build gemini`. Uses Google's official GenAI Go SDK at
`google.golang.org/genai`. The handler loop:

1. Builds `[]*genai.Content` from the persisted history window plus the new
   user text.
2. Calls `client.Models.GenerateContentStream(ctx, model, contents, cfg)`.
3. On each streamed `Candidate.Content.Part`:
   - `Text` → emit `AssistantEvent{Text}`.
   - `FunctionCall` → emit `AssistantEvent{ToolCall}` and return from the
     stage; the model's tool-call message is appended to `contents` so the
     subsequent `Resume(ToolResult)` will pair the `FunctionResponse` with
     it.
4. `FinishReason` → emit `AssistantEvent{FinalMessage}`.

The SDK choice — `google.golang.org/genai` rather than the older
`github.com/google/generative-ai-go` — picks the Google-supported forward path.
The `gemini` build tag isolates the dependency tree (gRPC, protobuf,
genproto) from the default binary; deployments that don't need the LLM ship
without it.

## Tool catalogue

| Name | Backend | Args | Approval default |
|---|---|---|---|
| `execute_script` | sandbox.Runner (Phase 3 container) | `{shell, script}` | required |
| `read_file` | fsops | `{path, max_bytes?}` | auto-granted |
| `list_dir` | fsops | `{path, depth?}` | auto-granted |
| `write_patch` | fsops | `{path, content, mode?, create?}` | required |
| `github_*` (~75 tools) | githubmcp (subprocess) | per-tool schema from upstream MCP server | required for every mutator (`create_`, `update_`, `delete_`, `merge_`, `push_`, …); read-only ops auto-granted |

The four base tools have schemas declared in `internal/middleware/tools.go`;
`github_*` schemas are fetched at orchestrator startup from the upstream
`github-mcp-server` binary and converted by `internal/githubmcp/schema.go`.
All schemas are SDK-agnostic — Gemini-specific conversion lives in
`gemini_tools.go` under the build tag. See [`github.md`](github.md) for the
GitHub MCP integration in depth.

### Why the fsops/sandbox split

`read_file`, `list_dir`, `write_patch` are pure filesystem ops on the
orchestrator's existing workspace directory (the same path the Docker
runner bind-mounts at `/work`). Running them through a container would add
hundreds of milliseconds of latency for ops that take microseconds
natively, with no security benefit — the trust boundary is identical
(orchestrator user owns the workspace either way).

Splitting also keeps `internal/sandbox` untouched: Phase 3's runner only
knows about `execute_script`.

`internal/fsops.Engine` enforces path safety: no absolute paths, no `..`
components (rejected up-front before any normalization), and symlinks must
resolve inside the workspace root.

## History

The orchestrator persists conversation turns so the LLM has context across
restarts. The schema is one row per turn keyed by `(sid, turn_idx)`:

```sql
CREATE TABLE IF NOT EXISTS turns (
    sid        TEXT    NOT NULL,
    turn_idx   INTEGER NOT NULL,
    role       TEXT    NOT NULL,      -- 'user'|'assistant'|'tool_call'|'tool_result'|'system.summary'
    parts_json BLOB    NOT NULL,
    ts         INTEGER NOT NULL,
    PRIMARY KEY (sid, turn_idx)
);
```

Backend choice via `NOMADDEV_HISTORY_BACKEND`:

- `sqlite` (default) — `modernc.org/sqlite`, pure Go, no cgo, single file at
  `NOMADDEV_HISTORY_PATH` (default `/var/lib/nomaddev/history.db`).
  PRAGMAs: `journal_mode=WAL`, `busy_timeout=5000`, `synchronous=NORMAL`.
- `memory` — process-local, lost on restart. Used in tests.

Windowing: `NOMADDEV_HISTORY_WINDOW_TURNS` (default 20) bounds how many
prior turns the middleware sends to the model. Turn-count is a coarse
proxy for tokens; token-aware windowing can land later.

History is **not** coupled to session reaping. The session ring buffer
(see `internal/session`) is wire-level reconnect-replay storage; history
is durable LLM context. Different layers, different durabilities.

### Background summarization compactor

Long-running sessions accumulate unbounded user/assistant turns, which
inflates Gemini context tokens on every `user.intent` (via `LoadWindow`)
and grows `history.db` on disk. An opt-in goroutine in
[`internal/history/summarizer.go`](../internal/history/summarizer.go)
walks every session on a tick and, when the cumulative word count of a
session's user + assistant turns crosses `WordThreshold`, asks a
configurable HTTP endpoint to summarize the oldest 50 % of those turns,
then replaces them with a single `system.summary` row inside one
transaction.

Properties:

- **No schema change.** The `system.summary` value is just another
  string in the existing `role TEXT` column — the Phase 8.7
  append-only migration list stays at Version 1.
- **Audit-safe.** `tool_call` / `tool_result` rows are never
  summarized; structured tool I/O stays intact.
- **Concurrency-safe.** Compaction acquires the same per-SID mutex
  that `Append` uses, so `turn_idx` stays monotonic against
  concurrent appends from the wsserver.
- **Failure-safe.** Any error from the summarizer leaves the database
  untouched; the next tick retries.
- **Idx-preserving.** The summary row reuses the smallest deleted
  `turn_idx`, so `LoadWindow`'s `ORDER BY turn_idx DESC` still
  returns turns in the right order. Subsequent `Append` calls
  continue past `MAX(turn_idx)+1` and never reuse gaps.
- **Wire-compatible.** Summary rows carry the same `{"text": "..."}`
  payload shape as user/assistant turns, so the translator's
  history-replay path needs no special-casing.

The endpoint is whatever the operator wires up — the orchestrator POSTs
a JSON array `[{"role": "user|assistant", "text": "...", "ts": <nanos>}]`
and expects `{"summary": "..."}` back. Disabled by default; configure
via `NOMADDEV_HISTORY_SUMMARY_*` env vars (see
[`docs/operations.md`](./operations.md#history-summarization-compactor)).

## Runtime selection

`NOMADDEV_MIDDLEWARE_RUNTIME`:

- `mock` (default in this build) — canned `AssistantEvent` stream, no API
  key needed.
- `gemini` — real Gemini calls; only works with `-tags gemini` (`make
  build-gemini`). Requires `NOMADDEV_GEMINI_API_KEY`.
- `none` — no middleware service attached; `user.intent` returns
  `error{not_implemented}`.

## Running the Gemini tests locally

```sh
NOMADDEV_GEMINI_API_KEY=$KEY make test-gemini
# or:
NOMADDEV_GEMINI_API_KEY=$KEY go test -tags gemini -count=1 ./internal/middleware/...
```

Each Gemini test calls `requireKey(t)` first; without the env var the suite
`t.Skip`s rather than failing. CI never sets the key.

## Future work (out of scope for Phase 4)

- Sidecar promotion: pull `internal/middleware` into a separate process at
  `cmd/middleware/` if the Gemini API key needs to be isolated from the
  orchestrator's blast radius.
- Token-aware history windowing — replace turn-count with a token budget once
  we trust the SDK's counter.
- Streaming tool calls — the SDK doesn't stream `FunctionCall` parts today;
  if that changes, the handler loop becomes a chunked-then-flush dance.
- More tools: long-running `execute_script` with progress reports, shell-aware
  redirection, etc.
