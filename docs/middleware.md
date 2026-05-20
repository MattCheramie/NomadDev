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
internal/fsops             Native filesystem ops for read_file/list_dir/write_patch/apply_code_patch
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
  and closes the turn. Carries `Usage{PromptTokens, CandidatesTokens,
  TotalTokens}` for the terminal stage (zero-valued when the SDK didn't
  report it).
- `Usage`: end-of-stage token accounting for a tool-call stage —
  emitted just before the channel closes so the handler can fold partial
  usage into the per-turn aggregate before `Resume`. Without this,
  tool-call legs would silently drop their token counts.
- `Err`: fatal turn error.

The wsserver aggregates `Usage` across every stage of a turn into a
single per-turn total, increments `nomaddev_llm_tokens_total{type=…}` on
each stage end (so retries that never reach the client still register
spend), and attaches the cumulative numbers to the terminal
`assistant.message.payload.usage`.

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
| `apply_code_patch` | fsops | `{file_path, search_string, replace_string}` | required (with dry-run diff preview in the ApprovalSheet) |
| `search_syntax` | sandbox.Runner (ast-grep `sg` in container) | `{pattern, lang?, path?, max_matches?, globs?}` | auto-granted (read-only) |
| `pin_file` | fsops read + `history.ReferenceBuffer` | `{path}` | auto-granted (read-only) |
| `unpin_file` | `history.ReferenceBuffer` | `{path}` | auto-granted (read-only) |
| `github_*` (~75 tools) | githubmcp (subprocess) | per-tool schema from upstream MCP server | required for every mutator (`create_`, `update_`, `delete_`, `merge_`, `push_`, …); read-only ops auto-granted |

The eight base tools have schemas declared in `internal/middleware/tools.go`;
`github_*` schemas are fetched at orchestrator startup from the upstream
`github-mcp-server` binary and converted by `internal/githubmcp/schema.go`.
All schemas are SDK-agnostic — Gemini-specific conversion lives in
`gemini_tools.go` under the build tag. See [`github.md`](github.md) for the
GitHub MCP integration in depth.

### Audit mode (dry-run)

A `user.intent` envelope may carry `mode: "audit"`. When set, the
orchestrator filters the per-turn tool catalogue handed to Gemini through
`Service.AvailableToolsFor("audit")`, dropping every mutating tool:
`execute_script`, `write_patch`, `apply_code_patch`, and every `github_*`
tool that `githubmcp.IsDestructiveTool` flags. `CompositeDispatcher` and
the wsserver per-call gate apply the same filter as defense-in-depth, so a
hallucinated tool name cannot bypass the restriction (the call is rejected
with `sandbox_unauthorized` / `sandbox_bad_request` and surfaces on the
wire as a normal `command.result`).

The system prompt is augmented with a steering line that tells the model
the catalogue has been narrowed to `read_file`, `list_dir`,
`search_syntax`, `pin_file`, `unpin_file`, and read-only `github_*` tools,
and that the expected deliverable is a markdown report rather than a
mutation. `pin_file` / `unpin_file` are not mutating — they only touch the
in-memory reference buffer — so they survive the audit-mode filter. Approval prompts
are never expected to fire in audit mode — there is nothing destructive
left in the catalogue to gate.

Unknown values for `mode` are rejected by the wsserver entry point with
`bad_envelope`; empty / omitted `mode` runs the unfiltered catalogue.

### Why the fsops/sandbox split

`read_file`, `list_dir`, `write_patch`, and `apply_code_patch` are pure
filesystem ops on the orchestrator's existing workspace directory (the
same path the Docker
runner bind-mounts at `/work`). Running them through a container would add
hundreds of milliseconds of latency for ops that take microseconds
natively, with no security benefit — the trust boundary is identical
(orchestrator user owns the workspace either way).

Splitting also keeps `internal/sandbox` untouched: Phase 3's runner only
knows about `execute_script`.

`internal/fsops.Engine` enforces path safety: no absolute paths, no `..`
components (rejected up-front before any normalization), and symlinks must
resolve inside the workspace root.

### `search_syntax` — structural code search via ast-grep

The orchestrator's read-only tools used to be limited to `read_file` and
`list_dir`; anything that resembled a *search* fell back to
`execute_script` wrapping `grep`/`rg`, which made every search round-trip
approval-gated and pushed the model into authoring brittle regex. Phase
12.x replaces that with `search_syntax`, a schema-typed tool that
invokes [ast-grep](https://ast-grep.github.io/) (`sg`) inside the
sandbox worker and returns a flat list of matches.

```jsonc
{
  "pattern": "fn $F($_: context.Context)",  // ast-grep meta-vars: $X / $$$X / $_
  "lang": "go",                              // optional language hint
  "path": "internal/middleware",             // optional sub-tree (relative)
  "max_matches": 100,                        // soft cap, hard ceiling 1000
  "globs": ["*.go", "!*_test.go"]            // optional glob filters
}
```

Response shape (also defined at
[`internal/sandbox/searchsyntax.go`](../internal/sandbox/searchsyntax.go)):

```jsonc
{
  "matches": [
    {"file": "internal/middleware/dispatcher.go",
     "line": 65, "column": 4, "end_line": 65, "end_column": 70,
     "snippet": "func (c *CompositeDispatcher) Dispatch(ctx context.Context, …)"}
  ],
  "total_matches": 12,
  "returned_matches": 12,
  "truncated": false,
  // When the envelope exceeds NOMADDEV_GITHUB_MAX_RESULT_BYTES the reshape
  // drops trailing matches and surfaces:
  //   "truncated": true,
  //   "original_bytes": 524288,
  //   "preview_bytes": 65324
}
```

Three things make the tool safe to leave un-gated:

1. **Read-only.** The sandbox container has no write side-effects on the
   bind-mount under `sg run`; the dispatcher does not route any mutating
   sg subcommand (`sg rewrite` etc. are not exposed).
2. **Bounded.** The middleware validator enforces `pattern ≤ 8 KiB`,
   `max_matches ≤ 1000`, no `..` in `path`, alpha-only `lang`. The
   sandbox runner caps captured stdout at 16 MiB before reshape, and the
   envelope is truncated to `NOMADDEV_GITHUB_MAX_RESULT_BYTES` before it
   hits the model.
3. **Container-isolated.** Same runtime / image policy as
   `execute_script`: distroless/alpine-based sandbox image, no network,
   read-only rootfs, gVisor when available. `sg` lives in the image
   (pre-baked via the Dockerfile `sandbox` target); the orchestrator
   itself never invokes `sg` on the host.

### `apply_code_patch` — surgical edit with a preview-gated approval

Unlike `write_patch`, which overwrites whole files, `apply_code_patch` performs
a single search/replace edit. The engine verifies `search_string` occurs
**exactly once** in the target — zero or multiple matches fail fast with
`sandbox.ErrBadRequest` — and computes a one-hunk unified diff before the
file is touched. The wsserver approval pipeline calls
`Engine.PreviewApplyCodePatch` to run this dry-run *before* sending the
`tool.approval.request` envelope, attaches the diff to the new
`preview` field, and the mobile ApprovalSheet renders it with `+`/`-`
colourisation so the operator approves the actual change rather than opaque
strings. The post-approval dispatch re-runs the same single-match check on
the freshly-read file to close the time-of-check/time-of-use window.

#### `verify_command` — apply, verify, rollback on failure (Phase 14)

`apply_code_patch` accepts an optional `verify_command` string. When set, the
dispatcher composes three steps into one tool call so the wire layer still
sees a single `command.request` → `command.result` envelope pair:

1. **Snapshot + apply.** `Engine.ApplyCodePatchWithSnapshot` writes the patch
   and returns the file's pre-edit bytes plus its resolved absolute path —
   everything the dispatcher needs to undo the write later.
2. **Verify.** The dispatcher invokes `Sandbox.Exec` with
   `tool=execute_script` and `script=verify_command` (shell=`bash`), reusing
   the calling session's `WorkingDir`, `Timeout`, `Limits`, and `SessionID`
   so the verify run lands in the same workspace as the patch.
3. **Decide.** Exit 0 leaves the patched file in place. Any non-zero exit OR
   any sandbox-side error (timeout / oom / dispatch failure) calls
   `Engine.RestoreFile` to revert the file to its snapshot, then appends a
   `verify_command failed (exit=N); rolled back <path>` notification to
   stderr and emits the verify command's own exit chunk.

The resulting `command.result` therefore carries the verify command's exit
code with no `SandboxErr*` code, which `ShouldAutoRetry` classifies as a
retryable shell failure. `BuildErrorReport` then feeds the verify stderr
(plus the rollback notification — both within the trailing 8 KiB window) to
the translator on its next stage, and the LLM authors a fix exactly the
way it would for any other failing tool call. The recovery budget treats a
verify-rollback the same as any other retryable failure: a success or a
non-retryable failure resets it; exhaustion escalates the chain to the
Mobile Control Hub as a `system.error_report` envelope.

The approval pipeline reads `verify_command` out of the call args and copies
it into the `preview` payload alongside `path` / `line_number` / `unified_diff`
so the ApprovalSheet renders a "Verify after apply — rollback on non-zero
exit" row before the operator hits Approve. A configured `verify_command`
requires a sandbox runner; dispatching without one fails fast with
`sandbox_bad_request` and never writes the patch.

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

### Persistent reference buffer

The compactor above is lossy by design: during a long, multi-step
execution chain it can summarize away a critical architectural file the
model read early on, leaving the tool dispatcher without context on core
data structures. The persistent reference buffer
([`internal/history/pins.go`](../internal/history/pins.go)) is the escape
hatch.

`ReferenceBuffer` is an in-memory, per-session map of pinned workspace
files — `sid -> path -> raw bytes` — kept **entirely separate from the
`Store` event log**, so the compactor never sees it. Two tools drive it:

- `pin_file` reads a file through the existing fsops `read_file` path
  (reusing its path-safety, symlink and byte-cap guarantees) and stores
  the raw contents under `(SID, path)`. Re-pinning the same path refreshes
  it.
- `unpin_file` drops a path from the buffer once the task no longer needs
  it, freeing the memory and prompt space it occupied. Unpinning an
  unpinned path is a clean no-op.

On every turn `runIntent` calls `ReferenceBuffer.Render(SID)` and prepends
the result — a `=== PINNED REFERENCE FILES ===` block — to the system
prompt, ahead of any audit-mode steering. Pinned files therefore stay in
full context no matter how aggressively history is compacted.

Properties:

- **In-memory only.** State is process-local and lost on restart; there
  is no SQLite table and no migration. A `reset_history` command clears a
  session's pins alongside its event log.
- **Bounded.** Conservative ceilings (256 KiB per file, 1 MiB aggregate
  per session, 32 files) keep the per-turn prompt cost in check; crossing
  one returns an `ErrPinCapExceeded` tool-result error so the model can
  `unpin_file` something.
- **Concurrency-safe.** A single mutex guards `Pin` / `Unpin` / `Render`.

## Runtime selection

`NOMADDEV_MIDDLEWARE_RUNTIME`:

- `mock` (default in this build) — canned `AssistantEvent` stream, no API
  key needed.
- `gemini` — real Gemini calls; only works with `-tags gemini` (`make
  build-gemini`). Requires `NOMADDEV_GEMINI_API_KEY`.
- `openai` / `anthropic` / `deepseek` — corresponding SDK-backed
  translators behind their own build tags.
- `none` — no middleware service attached; `user.intent` returns
  `error{not_implemented}`.

### Switching models at runtime

The active translator's model is chosen at startup via the per-runtime
env var (e.g. `NOMADDEV_OPENAI_MODEL`). Within one provider the mobile
Settings screen can switch models on the fly via
`user.command{action: "set_model", model: "<name>"}`:

- The hello envelope advertises `provider`, the current `model`, and an
  `available_models` catalogue sourced from `middleware.KnownModels()`.
- The server validates the requested model against the active provider's
  catalogue and stores it in an in-memory per-SID override map.
- The next `user.intent` picks up the new model via `TurnInput.Model`;
  in-flight turns are unaffected.
- `reset_history` clears the override alongside the conversation.
- The override is held in memory only — restart the orchestrator and
  every session goes back to its env-var default. The mobile client
  re-applies its remembered selection on reconnect by reading
  `HelloPayload.model`.

Cross-provider switching (e.g. openai → anthropic) is intentionally not
supported from the UI: it would require runtime API key changes and a
different build-tag binary.

## Automated error recovery (Phase 13)

When a middleware-dispatched tool call returns a retryable failure
(non-zero shell exit, `sandbox_timeout`, or `sandbox_oom`), the
orchestrator does **not** terminate the turn. Instead it formats the
captured stderr + exit code into a `event.SystemErrorReportPayload`,
stashes it under `ToolResult.Output["error_report"]`, and resumes the
translator so the LLM can author a fix as a new `command.request`.

The recovery state machine lives in `internal/middleware/recovery.go`:

- `middleware.ShouldAutoRetry(exitCode, errCode)` classifies a finished
  call. Non-zero exits and `sandbox_timeout` / `sandbox_oom` retry;
  `sandbox_bad_request`, `sandbox_unauthorized`, `sandbox_image_pull`,
  and `sandbox_canceled` are terminal because another LLM round can't
  fix them.
- `middleware.BuildErrorReport(call, exitCode, errCode, errMsg, stderr,
  attempt, maxAttempts)` assembles the payload, truncating stderr to
  `MaxErrorReportStderrBytes` (8 KiB) from the tail (the failing line is
  usually the last one).
- `middleware.NewRetryBudget(max)` is the per-turn counter. `Consume()`
  reports whether the chain still has budget after the current failure;
  `Reset()` is called after a success or a non-retryable failure so the
  budget tracks **consecutive** failures only.

The orchestration loop sits in `internal/wsserver/middleware.go`. On
each `runToolCall` result it consults `ShouldAutoRetry`; if budget
remains it enriches the resumed `ToolResult.Output`, otherwise it emits
a `system.error_report` envelope through `bufferAndSend` (the Mobile
Control Hub escalation) and closes the turn with
`finish_reason="error"`.

Configuration:

- `NOMADDEV_MAX_AUTORETRIES` (default `2`) — pipes through
  `config.MiddlewareConfig.MaxAutoRetries` →
  `middleware.RuntimeConfig.MaxAutoRetries`. Set to `0` to disable the
  loop entirely.

See `docs/events.md` § "Automated error recovery" for the wire-level
sequence diagram and `internal/middleware/recovery_test.go` /
`internal/wsserver/middleware_test.go` (`TestMiddleware_AutoRetry_*`)
for the canonical test cases.

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
