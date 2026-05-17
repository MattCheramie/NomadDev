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
| `EventUserIntent`     | `user.intent`       | C→S       | Free-text turn input for the NLP middleware. Payload: `{text, history_hint}`. |
| `EventAssistantChunk` | `assistant.chunk`   | S→C       | Streamed model text. Payload: `{seq, text}`. `correlation_id` = originating `user.intent.id`. |
| `EventAssistantMessage` | `assistant.message` | S→C     | Terminal frame for one turn — emitted exactly once. Payload: `{text, finish_reason, error}`. `text` may be empty when the model produced only tool calls. |
| `EventToolApprovalRequest` | `tool.approval.request` | S→C | Ask the human to authorize a destructive tool call. Payload: `{tool, args, reason, pending_command_id, timeout_ms}`. `correlation_id` = the pending `command.request.id`. |
| `EventToolApprovalGranted` | `tool.approval.granted` | C→S | Allow the pending tool call. `correlation_id` = the `tool.approval.request.id`. Empty payload. |
| `EventToolApprovalDenied`  | `tool.approval.denied`  | C→S | Refuse the pending tool call. Payload: `{reason}`. `correlation_id` = the `tool.approval.request.id`. |

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

**Example flow:**

```json
{"id":"01HX-r","type":"command.request","ts":"...","payload":{"tool":"execute_script","args":{"shell":"bash","script":"echo hi; echo err >&2"},"timeout_ms":5000}}
{"id":"01HX-c1","type":"command.chunk","ts":"...","correlation_id":"01HX-r","payload":{"stream":"stdout","seq":0,"data":"hi\n"}}
{"id":"01HX-c2","type":"command.chunk","ts":"...","correlation_id":"01HX-r","payload":{"stream":"stderr","seq":0,"data":"err\n"}}
{"id":"01HX-c3","type":"command.result","ts":"...","correlation_id":"01HX-r","payload":{"exit_code":0,"duration_ms":42}}
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
