# NomadDev Event Schema

All messages — in both directions — are JSON envelopes with this shape:

```json
{
  "id": "01HXYZABCDEFGHJKMNPQRSTUVW",
  "type": "ping",
  "ts": "2026-05-17T14:30:00.123456789Z",
  "correlation_id": "01HXYZ...",
  "payload": { }
}
```

| Field            | Type   | Notes |
|------------------|--------|-------|
| `id`             | string | ULID. Sortable and monotonic per source. |
| `type`           | string | One of the constants in `internal/event/types.go`. |
| `ts`             | string | RFC 3339 with nanoseconds. |
| `correlation_id` | string | Optional. Set on replies to link them to the originating envelope. |
| `payload`        | object | Optional. Type-specific schema. |

## Type catalogue

| Constant              | Wire string         | Direction | Purpose |
|-----------------------|---------------------|-----------|---------|
| `EventHello`          | `hello`             | S→C       | Sent immediately after upgrade. |
| `EventClientHello`    | `client.hello`      | C→S       | Reconnect handshake with `last_event_id`. |
| `EventAck`            | `ack`               | S→C       | Acknowledges a received event. |
| `EventPing`           | `ping`              | both      | App-layer heartbeat. |
| `EventPong`           | `pong`              | both      | Reply to `ping`. |
| `EventError`          | `error`             | S→C       | Structured error with `code` + `message`. |
| `EventSessionStale`   | `session.stale`     | S→C       | Buffer rolled past `last_event_id`. |
| `EventSessionReplaced`| `session.replaced`  | S→C       | A newer connection took over the SID. |
| `EventCommandRequest` | `command.request`   | C→S, S→C  | Run a sandbox tool. Payload: `{tool, args, working_dir, timeout_ms}`. Clients send this directly; the middleware also mints it when the LLM emits a tool call. |
| `EventCommandChunk`   | `command.chunk`     | S→C       | Live stdout/stderr slice. Payload: `{stream, seq, data}`. `correlation_id` = originating `command.request.id`. |
| `EventCommandResult`  | `command.result`    | S→C       | Terminal summary — emitted exactly once per request. Payload: `{exit_code, duration_ms, error, error_message}`. |
| `EventSandboxHeartbeat` | `sandbox.heartbeat` | S→C     | Liveness ping emitted during stretches of stdout/stderr silence. Payload: `{elapsed_ms}`. `correlation_id` = originating `command.request.id`. Best-effort and idempotent — a missed heartbeat is harmless; see [Streaming semantics for `command.request`](#streaming-semantics-for-commandrequest). |
| `EventUserIntent`     | `user.intent`       | C→S       | Free-text turn input for the NLP middleware. Payload: `{text, history_hint, mode, images?}`. `mode == "audit"` is the optional dry-run flag: the orchestrator strips every mutating tool (`execute_script`, `write_patch`, `apply_code_patch`, destructive `github_*`) from the catalogue before the schema reaches Gemini, and the dispatcher refuses to run them defense-in-depth — the assistant can only call `read_file`, `list_dir`, `search_syntax`, and read-only `github_*` tools (typically to draft a markdown report). Empty / omitted `mode` runs the normal full catalogue. `images` is an optional array of `{media_type, data}` attachments (base64 bytes, no `data:` prefix); the orchestrator enforces `NOMADDEV_USER_INTENT_MAX_IMAGES` / `NOMADDEV_USER_INTENT_MAX_IMAGE_BYTES` and `media_type` is restricted to `image/jpeg`, `image/png`, `image/gif`, `image/webp`. Each translator backend wraps them as its SDK's native vision content block (Gemini `InlineData`, Anthropic `ImageBlock`, OpenAI `image_url` content part). Vision capability is checked against `internal/middleware/pricing/capabilities.go`: when the active `(provider, model)` is known text-only (e.g. `openai/o3-mini`, `deepseek/deepseek-chat`) the orchestrator rejects up-front with `bad_envelope` so the client sees a clear "switch to a vision-capable model" diagnostic. Anything else returns a `bad_envelope` error. |
| `EventAssistantChunk` | `assistant.chunk`   | S→C       | Streamed model text. Payload: `{seq, text}`. `correlation_id` = originating `user.intent.id`. |
| `EventAssistantThinking` | `assistant.thinking` | S→C    | Streamed slice of the model's internal reasoning. Payload: `{seq, text}`. Currently emitted only when the Anthropic backend has extended thinking enabled via `NOMADDEV_ANTHROPIC_THINKING_BUDGET >= 1024`. Sequence numbers are independent from `assistant.chunk.seq` so clients can render the two streams in parallel; thinking frames are NOT folded into `assistant.message.text`. `correlation_id` = originating `user.intent.id`. |
| `EventAssistantMessage` | `assistant.message` | S→C     | Terminal frame for one turn — emitted exactly once. Payload: `{text, finish_reason, error, usage?}`. `text` may be empty when the model produced only tool calls. `usage` carries cumulative LLM token accounting for the whole turn (summed across every translator stage, tool-call legs included): `{prompt_tokens, candidates_tokens, total_tokens, cost_usd?}`. `cost_usd` is the estimated dollar cost derived from the compiled-in price table at `internal/middleware/pricing/`; it is omitted when zero (no spend, or no entry for the active `(provider, model)`). Omitted entirely on error frames and when the translator reported no counts; the Mobile Control Hub folds it into a per-session 'Session Cost' ticker in the Settings drawer. |
| `EventToolApprovalRequest` | `tool.approval.request` | S→C | Ask the human to authorize a destructive tool call. Payload: `{tool, args, reason, pending_command_id, timeout_ms, preview?}`. `correlation_id` = the pending `command.request.id`. `preview` is an optional tool-specific dry-run payload (e.g. `apply_code_patch` attaches `{path, line_number, unified_diff}`, plus `verify_command` when set so the operator sees what will run after the patch lands and that a non-zero exit triggers an automatic rollback); see `docs/approval.md`. |
| `EventToolApprovalGranted` | `tool.approval.granted` | C→S | Allow the pending tool call. `correlation_id` = the `tool.approval.request.id`. Empty payload. |
| `EventToolApprovalDenied`  | `tool.approval.denied`  | C→S | Refuse the pending tool call. Payload: `{reason}`. `correlation_id` = the `tool.approval.request.id`. |
| `EventSystemErrorReport` | `system.error_report` | S→C | Sent to the Mobile Control Hub when the middleware exhausts `NOMADDEV_MAX_AUTORETRIES` auto-fix attempts on a failing tool call. Payload: `{tool, original_call_id, exit_code, error_code, error_message, stderr, attempt, max_attempts, escalated:true}`. `correlation_id` = the originating `user.intent.id`. The same payload shape (with `escalated:false`) is also stashed into `ToolResult.Output["error_report"]` and fed to the translator on each intermediate retry — see "Automated error recovery" below. |

## Example payloads

**hello (S→C):**
```json
{"id":"01HX...","type":"hello","ts":"...","payload":{"session_id":"sess-1","server_time":"...","protocol_version":1}}
```

**client.hello (C→S):**
```json
{"id":"01HX...","type":"client.hello","ts":"...","payload":{"last_event_id":"01HX..."}}
```

**ping / pong:**
```json
{"id":"01HX...","type":"ping","ts":"...","payload":{"nonce":"abc"}}
{"id":"01HX...","type":"pong","ts":"...","correlation_id":"<ping id>","payload":{"nonce":"abc"}}
```

**error:**
```json
{"id":"01HX...","type":"error","ts":"...","correlation_id":"<offending id>","payload":{"code":"unknown_type","message":"unsupported event type: foo"}}
```

Defined error codes: `unknown_type`, `bad_envelope`, `not_implemented`, `internal`, `unauthorized`.

## Streaming semantics for `command.request`

A single `command.request` produces zero or more `command.chunk` envelopes
followed by **exactly one** `command.result`. All three share the request's
`id` via `correlation_id`.

- Chunks are best-effort live frames. Under buffer pressure they may be evicted
  from the ring buffer between disconnect and reconnect; the `command.result`
  is the durable record of what happened.
- `seq` is per-stream (one counter for stdout, one for stderr) so a client can
  detect gaps inside one stream without correlating across both.
- `data` is utf-8 — invalid byte sequences are replaced with U+FFFD on the
  server side. A future tool that needs raw bytes can grow a sibling `data_b64`
  field; the schema reserves the name.
- A clean process exit produces `command.result.error == ""` and the real
  `exit_code`. Sandbox-side failures produce `exit_code == -1` and one of:
  `sandbox_timeout`, `sandbox_oom`, `sandbox_image_pull`, `sandbox_unavailable`,
  `sandbox_bad_request`, `sandbox_canceled`, `sandbox_internal`.
- `sandbox.heartbeat` envelopes may be interleaved with `command.chunk`
  envelopes while the request is in flight. They are emitted by the
  wsserver on a configurable interval (see
  `NOMADDEV_SANDBOX_HEARTBEAT_INTERVAL` in `docs/sandbox.md`) when the
  container has been silent for one interval; the ticker is reset on every
  real chunk so chatty jobs don't double-emit. They are best-effort —
  share the per-client outbound drop policy — and idempotent. Clients
  use them to drive a "still alive" elapsed timer on the Live Terminal.

**Example flow:**

```json
{"id":"01HX-r","type":"command.request","ts":"...","payload":{"tool":"execute_script","args":{"shell":"bash","script":"sleep 6; echo hi"},"timeout_ms":10000}}
{"id":"01HX-h1","type":"sandbox.heartbeat","ts":"...","correlation_id":"01HX-r","payload":{"elapsed_ms":5000}}
{"id":"01HX-c1","type":"command.chunk","ts":"...","correlation_id":"01HX-r","payload":{"stream":"stdout","seq":0,"data":"hi\n"}}
{"id":"01HX-c2","type":"command.result","ts":"...","correlation_id":"01HX-r","payload":{"exit_code":0,"duration_ms":6020}}
```

## Phase 4 turn flow

A single `user.intent` from the client triggers a turn the middleware runs to
completion. The flow is:

```
client                                 orchestrator + middleware
──────                                 ─────────────────────────
user.intent(id=U) ────────────────▶
                                       (translator opens stream)
                  ◀──────────────── assistant.chunk(seq=0, correlation_id=U)
                  ◀──────────────── assistant.chunk(seq=1, correlation_id=U)
                                       (LLM emits a tool call)
                  ◀──────────────── command.request(id=C, correlation_id=U)
                  ◀──────────────── tool.approval.request(id=A, correlation_id=C)
tool.approval.granted(corr=A) ────▶
                                       (dispatcher runs the tool)
                  ◀──────────────── command.chunk(correlation_id=C)*
                  ◀──────────────── command.result(correlation_id=C)
                                       (translator resumes with the result)
                  ◀──────────────── assistant.message(correlation_id=U, finish_reason)
```

Correlation rules:

- `assistant.chunk` and `assistant.message` `correlation_id` → the originating
  `user.intent.id`.
- `command.request` minted by the middleware carries `correlation_id` = the
  same `user.intent.id`, so a client grouping by "earliest ancestor with empty
  `correlation_id`" recovers the full turn.
- `tool.approval.request` `correlation_id` → the pending `command.request.id`.
- `tool.approval.granted` / `denied` `correlation_id` → the
  `tool.approval.request.id`.

A denied or timed-out approval still produces a terminal `command.result` with
`error: "sandbox_unauthorized"` so the wire record is complete and replay-safe.

Defined `command.result.error` codes (set on sandbox-side failure with
`exit_code == -1`): `sandbox_timeout`, `sandbox_oom`, `sandbox_image_pull`,
`sandbox_unavailable`, `sandbox_bad_request`, `sandbox_internal`,
`sandbox_canceled`, `sandbox_unauthorized`.

## Automated error recovery

When a middleware-dispatched tool call returns a retryable failure
(non-zero exit, `sandbox_timeout`, or `sandbox_oom`), the orchestrator
does **not** immediately terminate the turn or pause for human input.
Instead it:

1. Captures the failing tool's stderr (truncated to 8 KiB from the tail).
2. Formats a `SystemErrorReportPayload` (`tool`, `original_call_id`,
   `exit_code`, `error_code`, `error_message`, `stderr`, `attempt`,
   `max_attempts`, `escalated:false`).
3. Stashes the payload under `ToolResult.Output["error_report"]` and
   resumes the translator. The LLM is expected to read the structured
   error and author a fix as a new `command.request`.

The retry budget is bounded by `NOMADDEV_MAX_AUTORETRIES` (default `2`)
and scoped **per tool-call chain**: a success or a non-retryable failure
(`sandbox_bad_request`, `sandbox_unauthorized`, `sandbox_image_pull`,
`sandbox_canceled`) resets the counter so a sporadic transient doesn't
burn budget for the rest of a multi-step turn.

When the budget is exhausted, the orchestrator emits a
`system.error_report` envelope to the Mobile Control Hub with the same
payload shape and `escalated:true`, then terminates the turn with
`assistant.message{finish_reason:"error"}`. The wire envelope's
`correlation_id` is the originating `user.intent.id` so the mobile UI
can attribute the failure to the right turn.

```
client                          orchestrator + middleware
──────                          ─────────────────────────
user.intent(id=U) ─────────▶
                                (translator emits ToolCall)
              ◀────────── command.request(id=C1, correlation_id=U)
              ◀────────── command.chunk*(stderr)
              ◀────────── command.result(exit_code=1, correlation_id=C1)
                                (ShouldAutoRetry → enrich + resume)
                                (translator emits a fix ToolCall)
              ◀────────── command.request(id=C2, correlation_id=U)
              ◀────────── command.result(exit_code=1, correlation_id=C2)
                                ... up to NOMADDEV_MAX_AUTORETRIES ...
              ◀────────── system.error_report(correlation_id=U, escalated:true)
              ◀────────── assistant.message(correlation_id=U, finish_reason="error")
```

`MaxAutoRetries=0` disables the loop — the first retryable failure
escalates immediately.
