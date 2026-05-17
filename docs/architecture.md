# NomadDev Architecture

```
+--------------+      Tailscale       +-------------------+
| RN Mobile    | <----------------->  | Orchestrator (Go) |
| (Phase 5)    |   WSS, JWT, JSON     | cmd/orchestrator  |
+--------------+                      +---------+---------+
                                                |
                                  in-process    |  function-call
                                                v
                                      +-------------------+
                                      | Middleware (Go)   |
                                      | internal/middleware
                                      | (Gemini, Phase 4) |
                                      +---------+---------+
                                                |  command.request
                                                v
                                      +-------------------+
                                      | Sandbox Runner    |
                                      | cmd/sandbox       |
                                      | (Docker + gVisor, |
                                      |  Phase 3)         |
                                      +-------------------+
```

## Trust boundaries

1. **Public internet ↔ Tailscale.** The orchestrator binds only to the
   Tailscale interface (`100.x.y.z:8080`) on the host. No public port is open.
2. **Client ↔ Orchestrator.** WebSocket upgrade is gated on a JWT (HS256).
   See `docs/auth.md`.
3. **Orchestrator ↔ Middleware.** In-process today; can be moved behind a
   loopback gRPC boundary if the Gemini API key needs to be isolated.
4. **Middleware ↔ Sandbox.** All tool calls cross into ephemeral containers
   with gVisor; the host filesystem is not visible.
5. **Human-in-the-loop.** Destructive tool calls (`write_patch`,
   `execute_script` with elevated args) require a biometric approval round-trip
   to the mobile client before they execute.

## Event flow (Phase 2 baseline)

```
client ──ws──> orchestrator
   |              | upgrade gated on JWT
   |              | wsserver writes `hello` to client (and to ring buffer)
   |              |
   |── ping ────> | readPump
   |              | writePump emits `pong` with matching correlation_id
   |              |
   |── client.hello{last_event_id} ──> session.EventsSince(...) replays buffered envelopes
```

Phase 3 adds `command.request` → `command.result` over the same channel.
Phase 4 makes the orchestrator emit `command.request` itself in response to
free-text user input. Phase 5 is the UI for all of the above.
