# NomadDev Architecture

```
+--------------+      Tailscale       +-------------------------+
| Browser /    | <----------------->  | Orchestrator (Go)       |
| Mobile WebView|  WSS + HTTPS         | cmd/orchestrator        |
+--------------+   JWT, JSON          |                         |
                                      |  GET /     ─┐           |
                                      |  GET /...  ─┤ Hosted    |
                                      |             │ SPA       |
                                      |  GET /ws   ─┘  (Phase 5)|
                                      +---------+---------------+
                                                |
                                  in-process    |  function-call
                                                v
                                      +-------------------+
                                      | Middleware (Go)   |
                                      | internal/middleware
                                      | (Gemini, in-process)|
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

Phase 5 lands the SPA in the **same binary**: the Expo project at `mobile/`
is exported as static web assets via `expo export --platform web` and
embedded via `//go:embed all:dist` from `internal/wsserver/spa.go`. The
`/` and `/<route>` routes serve the bundle; `/ws` and `/healthz` keep
their handlers because `net/http.ServeMux` longest-prefix wins keeps
specific paths from being eaten by the catch-all. See `docs/mobile.md`.

Phase 2 ships the WebSocket relay. Phase 3 adds the in-process sandbox
runner (`internal/sandbox.Runner`), wired in at
`internal/wsserver/sandbox.go`. Phase 4 plugs Gemini between the client and
the runner: `internal/middleware` translates `user.intent` envelopes into
typed tool calls and dispatches them through either
`sandbox.Runner` (for `execute_script` and `search_syntax`) or
`internal/fsops` (for `read_file` / `list_dir` / `write_patch` /
`apply_code_patch`). Persistent LLM history lives in
`internal/history` (SQLite by default) — a separate concern from the
session ring buffer in `internal/session`, which handles wire-level
reconnect replay.

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
   |   <── sandbox.heartbeat* during stretches of stdout/stderr silence,
   |   <── command.result  then exactly one command.result on exit.
   |              |
   |── user.intent ──> handleUserIntent drives the translator loop:
   |   <── assistant.chunk*  streamed model text,
   |              |          when the model emits a tool call →
   |   <── command.request   server-minted (correlation_id = user.intent.id),
   |   <── tool.approval.request (if RequiresApproval(tool, args))
   |── tool.approval.granted ──> dispatch through CompositeDispatcher
   |   <── command.chunk* / command.result
   |              |          then resume the translator with the tool result,
   |   <── assistant.message  terminal frame for the turn.
```

Phase 5 ships the UI: an Expo + TypeScript SPA at `mobile/` that consumes
the wire protocol described above and renders it as native cards rather
than raw envelopes. The SPA is built once, embedded into the
orchestrator, and served at `/` from the same listener as `/ws`.
