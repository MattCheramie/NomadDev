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
| `EventCommandRequest` | `command.request`   | C→S       | Phase 3 placeholder — Phase 2 returns `error{code:"not_implemented"}`. |
| `EventCommandResult`  | `command.result`    | S→C       | Phase 3 placeholder. |

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
