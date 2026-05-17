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
                                                |  Runner.Exec
                                  in-process    v
                                      +-------------------+
                                      | Sandbox Runner    |
                                      | internal/sandbox  |
                                      | (Docker + gVisor) |
                                      | cmd/sandbox is a  |
                                      | debug CLI         |
                                      +-------------------+
```

In Phase 2 the orchestrator hosts the WebSocket relay. Phase 3 ships the
sandbox runner in-process (`internal/sandbox.Runner`); the orchestrator calls
it directly from `internal/wsserver/sandbox.go`. The Middleware box is still
notional — Phase 4 will plug Gemini between the client envelope and the
runner call.

## Trust boundaries

1. **Public internet ↔ Tailscale.** The orchestrator binds only to the
   Tailscale interface (`100.x.y.z:8080`) on the host. No public port is open.
2. **Client ↔ Orchestrator.** WebSocket upgrade is gated on a JWT (HS256).
   See `docs/auth.md`.
3. **Orchestrator ↔ Middleware.** In-process today; can be moved behind a
   loopback gRPC boundary if the Gemini API key needs to be isolated.
4. **Middleware ↔ Sandbox.** Tool calls run in ephemeral one-shot containers
   with `NetworkMode=none` and `ReadonlyRootfs=true`. gVisor (`runsc`) is
   preferred when the Docker daemon advertises it and falls back to the
   default runtime with a warn log otherwise. The only writable host path is
   the bind-mounted workspace at `/work`. See `docs/sandbox.md` for the full
   threat model.
5. **Human-in-the-loop.** Destructive tool calls (`write_patch`,
   `execute_script` with elevated args) require a biometric approval round-trip
   to the mobile client before they execute.

## Event flow

```
client ──ws──> orchestrator
   |              | upgrade gated on JWT
   |              | wsserver writes `hello` to client (and to ring buffer)
   |              |
   |── ping ────> | readPump
   |              | writePump emits `pong` with matching correlation_id
   |              |
   |── client.hello{last_event_id} ──> session.EventsSince(...) replays buffered envelopes
   |              |
   |── command.request ──> handleCommandRequest spawns goroutine,
   |              |        Runner.Exec streams ExecChunks back as
   |   <── command.chunk*  command.chunk envelopes (utf-8, per-stream seq),
   |   <── command.result  then exactly one command.result on exit.
```

Phase 4 makes the orchestrator emit `command.request` itself in response to
free-text user input. Phase 5 is the UI for all of the above.
