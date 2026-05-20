# Sandbox runner

Phase 3 of NomadDev gives the orchestrator the ability to run scripts inside
one-shot containers and stream their output back over the same WebSocket the
mobile client is already on. This document covers the design, the threat
model, and how to flip between mock / Docker / disabled at runtime.

## Architecture

```
client ──ws──▶ orchestrator ──Runner.Exec──▶ {mock, docker} ──▶ container
   ▲              │                                              │
   │              ▼                                              ▼
   └────ws────── command.chunk ◀──────────── stdout / stderr ────┘
                command.result (terminal, exactly once)
```

A `command.request` envelope enters `wsserver.handleCommandRequest`. That
function validates the payload, acquires a per-server semaphore slot
(NOMADDEV_SANDBOX_MAX_CONCURRENT), spawns a goroutine that calls
`sandbox.Runner.Exec`, and pipes each `ExecChunk` into a `command.chunk`
envelope on the existing `bufferAndSend` path so reconnect replay works for
free. The terminal `Stream==exit` chunk is rendered as a single
`command.result` envelope.

A watchdog goroutine listens on `Client.Done()` and cancels the per-exec
context if the client disconnects mid-run; the Docker runner kills its
container, the mock runner exits its loop, and either way a final
`command.result{error: "sandbox_canceled"}` lands in the ring buffer so a
reconnecting client sees the cancellation rather than a hung tile.

## Threat model

The Docker runner's defaults are intentionally paranoid:

- **No network** (`NetworkMode=none`). The container cannot reach the
  internet, the host's network, or other containers. Configurable but the
  default is "off".
- **Read-only root filesystem**. The only writable host path is the
  bind-mounted workspace at `/work`, plus an in-container tmpfs at `/tmp`
  capped at 64 MiB.
- **gVisor when available**. `NewDockerRunner` probes the daemon for the
  `runsc` runtime and selects it when present. Without gVisor the container
  uses the host kernel directly — same level of isolation as any other
  Docker container, lower than gVisor's intercepted-syscall model.
- **Hard resource caps**: `Memory`, `NanoCPUs`, `PidsLimit`. Defaults are
  256 MiB / 1 CPU / 256 pids.
- **Explicit cleanup via `defer ContainerRemove(force: true)`**. `AutoRemove`
  is intentionally **off** so the runner can `ContainerInspect` the exited
  container and read `State.OOMKilled` before cleanup. The deferred force
  remove runs on every path — success, timeout, ctx cancel, and the
  create-then-start failure path — so no container is ever leaked.
- **Single tool, single shape**. Phase 3 only accepts
  `tool="execute_script"` with `args.script` (and optional `args.shell`).
  Anything else returns `sandbox_bad_request` immediately without touching
  the engine.
- **Image digest pinning**. `NOMADDEV_SANDBOX_IMAGE` accepts a
  digest-pinned ref (`alpine:3.20@sha256:…`). When pinned, Docker
  verifies the digest at pull time AND the runner re-inspects the local
  image before every exec and refuses to start the container if
  `RepoDigests` no longer matches — this catches a host-local
  `docker tag` smuggling a different manifest under the configured name.
  Set `NOMADDEV_SANDBOX_REQUIRE_DIGEST=true` to hard-fail at boot on a
  tag-only ref (recommended for production).

## Resource limits

| Knob | Default | Docker field |
|---|---|---|
| `NOMADDEV_SANDBOX_MEMORY` | 256 MiB | `HostConfig.Memory` (bytes) |
| `NOMADDEV_SANDBOX_NANOCPUS` | 1.0 CPU | `HostConfig.NanoCPUs` (nano-CPUs) |
| `NOMADDEV_SANDBOX_PIDS_LIMIT` | 256 | `HostConfig.PidsLimit` |
| `NOMADDEV_SANDBOX_DEFAULT_TIMEOUT` | 30s | wall-clock; overridable per-request via `timeout_ms` |
| `NOMADDEV_SANDBOX_MAX_CONCURRENT` | 4 | per-server semaphore; `0` = unlimited |
| `NOMADDEV_SANDBOX_NETWORK` | `none` | `HostConfig.NetworkMode` |
| `NOMADDEV_SANDBOX_READONLY_ROOTFS` | `true` | `HostConfig.ReadonlyRootfs` |
| `NOMADDEV_SANDBOX_PREFER_RUNSC` | `true` | `HostConfig.Runtime` when daemon advertises `runsc` |
| `NOMADDEV_SANDBOX_IMAGE` | `alpine:3.20` | container image (override with the ast-grep-enabled image — see [Worker image with ast-grep](#worker-image-with-ast-grep)) |
| `NOMADDEV_SANDBOX_WORKSPACE_DIR` | `/var/lib/nomaddev/work` | host path bind-mounted at `/work` |
| `NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE` | `false` | Phase 10.2: per-SID workspace subdir |
| `NOMADDEV_SANDBOX_HEARTBEAT_INTERVAL` | `5s` | wsserver emits `sandbox.heartbeat` envelopes during stretches of stdout/stderr silence so the Live Terminal in the Mobile Control Hub can render an "still alive" elapsed timer. The ticker is reset whenever a real `command.chunk` forwards, so chatty jobs don't double-emit. Set `0s` to disable. |

### Live Terminal heartbeats

`sandbox.heartbeat` is emitted from `wsserver.runExec`, not the runner —
the runner stays a pure stdout/stderr producer. The wsserver consumes
the runner's `<-chan ExecChunk` in a `select` over the channel and a
`time.NewTicker(NOMADDEV_SANDBOX_HEARTBEAT_INTERVAL)`; every real chunk
calls `ticker.Reset(...)` so an active job never produces a heartbeat,
and the ticker is `Stop()`'d before the terminal `command.result` so no
heartbeat ever races past the result envelope. The payload is just
`{elapsed_ms}`; `correlation_id` is the originating `command.request.id`.

Heartbeats are best-effort: they share the per-client 64-slot
`bufferAndSend` drop policy. A missed heartbeat is harmless — the next
one (or the next real chunk) refreshes the client's elapsed timer.
Avoid intervals below `1s` under load.

### Total-resource budgeting (Phase 10 doc)

The sandbox enforces per-run caps, not a host-wide budget. With
defaults (`MEMORY=256MiB`, `MAX_CONCURRENT=4`), the worst-case
memory footprint is `MAX_CONCURRENT × MEMORY` = **1 GiB** of
container RSS, plus the orchestrator process itself (~50 MiB
steady state) and any cached SPA bundle. Size the host accordingly:

| Profile | `MAX_CONCURRENT` | `MEMORY` | Worst-case container RSS |
|---|---|---|---|
| Single-tenant dev | 4 | 256 MiB | 1 GiB |
| Hetzner CX22 (2 GiB) | 4 | 256 MiB | 1 GiB (50% headroom) |
| Hetzner CAX11 (4 GiB) | 8 | 384 MiB | 3 GiB |
| Multi-tenant (≥ 4 GiB) | 8 | 256 MiB + per-session WS | 2 GiB |

A "total memory pool" model (track allocations across runs, refuse
new starts when sum exceeds budget) is an architecturally bigger
change than the per-run caps; the documented sizing approach
covers the same blast radius for any realistic deploy.

### Per-session workspace isolation (Phase 10.2)

Without `NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE=true`, every
session shares the same `<WorkspaceDir>` bind-mount at `/work`. A
multi-tenant deploy where two operators run scripts that touch
`/work/foo` will see each other's files. Setting the knob to true
makes the runner bind-mount `<WorkspaceDir>/<sanitized-sid>/`
instead (mode 0o700, created on first use). The SID is sanitized
to a path-safe identifier (alphanumerics, `-_.`, capped at 64
bytes) before joining; `..` traversal is collapsed to `__`.

**Phase 12.2:** `fsops` (read_file / write_patch / list_dir) now
honors the same `NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE` flag —
sandbox and fsops isolate in lockstep. The middleware dispatcher
attaches the calling session id via `fsops.WithSessionID(ctx, sid)`
before invoking `Engine.Run`; the engine routes paths through
`<root>/<sanitized-sid>/` (created at 0o700 on first use). The
known limitation noted in Phase 10.2 is closed.

### Host-side `git`: the worker pool (Phase 15)

The optional Phase 15 worker pool (`dispatch_worker_pool` — see
[`docs/middleware.md`](middleware.md#dispatch_worker_pool--concurrent-worktree-migration-phase-15))
is the one feature in NomadDev that steps **outside** the Docker sandbox
boundary described above. Worktree add/remove, commit, and merge have no
container equivalent, so when `NOMADDEV_WORKER_POOL_ENABLED=true` the
orchestrator drives the host `git` binary directly through the
`internal/gitctl` package.

`gitctl` runs `git` against `NOMADDEV_SANDBOX_WORKSPACE_DIR`, which in
this mode must be a **pre-cloned git repo**. The isolated worktrees the
pool creates live at `<workspace>/.nomaddev-worktrees/<id>` — one
directory per sub-task, removed once the pool finishes. This is host
filesystem and host process activity, not a sandboxed container run.

Because that crosses the sandbox boundary, every `gitctl` invocation is
hardened:

- `-c core.hooksPath=/dev/null` — repo-supplied git hooks are
  attacker-influenced; running one would be host RCE. They never run.
- `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null` — no
  system- or user-level git config is consulted.
- `GIT_TERMINAL_PROMPT=0` — git never blocks waiting for terminal
  input.
- A fixed argv with no shell interpolation.

The feature is opt-in and disabled by default precisely because it
grants this host-side privilege; see
[`SECURITY.md`](../SECURITY.md) for the security note. The worker pool
also requires `NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE=false` — the
per-session subdir layout above is incompatible with the shared-root git
repo `gitctl` operates on, and the tool returns a clear error otherwise.

### User-namespace remapping (Phase 10 doc)

Docker's [`userns-remap`](https://docs.docker.com/engine/security/userns-remap/)
maps container UIDs to a sub-range on the host so a process
running as `root` inside the container is an unprivileged user
outside it. Stronger isolation than the default
`NoNewPrivileges` + `ReadonlyRootfs` posture, and pairs well with
the per-session workspace from above.

NomadDev doesn't control daemon config from within the
orchestrator — set this once per host:

```sh
# /etc/docker/daemon.json
{
  "userns-remap": "default"
}
```

then `sudo systemctl restart docker`. The Docker daemon creates a
`dockremap` user with a 65536-uid sub-range; container root maps
to `dockremap`'s `subuid` start, which has no privileges on the
host. **Note:** the orchestrator's bind-mounted `WorkspaceDir`
needs to be writable by that mapped UID, which usually means
either creating it owned by `dockremap` (`chown 100000:100000`)
or running the orchestrator as `dockremap` itself. The systemd
unit's `User=nomaddev` runs as a different uid by default; the
operator picks one or the other.

## Worker image with ast-grep

The `search_syntax` tool (Phase 12.x) invokes [`sg`](https://ast-grep.github.io/)
inside the sandbox container. The default `alpine:3.20` image doesn't
ship with it, and `NOMADDEV_SANDBOX_NETWORK=none` (the default) blocks
runtime `apt`/`apk` installs, so the binary has to be pre-baked into the
image. ast-grep upstream only publishes glibc-linked binaries, so the
dedicated worker image uses `debian:bookworm-slim` rather than alpine —
the orchestrator's own container (Stage 4) stays distroless/alpine-built.

The repo's [`Dockerfile`](../Dockerfile) carries a dedicated `sandbox`
target for exactly this:

```bash
docker build --target sandbox -t nomaddev/sandbox:bookworm-sg .
```

Then point the orchestrator at it:

```bash
NOMADDEV_SANDBOX_IMAGE=nomaddev/sandbox:bookworm-sg ./orchestrator
```

If you'd rather extend your own base image, the only requirement is that
`sg` is on `PATH` inside the container. The upstream release zip carries
both `ast-grep` and `sg` (the short-form CLI the search_syntax handler
invokes):

```dockerfile
FROM your-debian-base:tag
ARG AST_GREP_VERSION=0.42.2
RUN curl -fsSL -o /tmp/ag.zip \
      "https://github.com/ast-grep/ast-grep/releases/download/${AST_GREP_VERSION}/app-x86_64-unknown-linux-gnu.zip" \
 && unzip -o /tmp/ag.zip -d /usr/local/bin/ \
 && chmod +x /usr/local/bin/ast-grep /usr/local/bin/sg \
 && rm /tmp/ag.zip
```

On alpine you'd need a glibc-compat layer (e.g. `gcompat`) or build
ast-grep from source against musl, since upstream doesn't ship a
musl-linked release. The Debian path is the documented one.

When `sg` is missing the tool still degrades gracefully: the container
exits non-zero with `sg: not found` on stderr, the runner surfaces that
in the `error` field of the envelope, and the model sees a `sandbox.err.*`
result rather than a panic.

The envelope returned to the model is capped by
`NOMADDEV_GITHUB_MAX_RESULT_BYTES` (default 1 MiB, shared with the
GitHub MCP backend) so a permissive pattern like `$X` against a large
tree can't blow the model's context window.

## Runtime selection

`NOMADDEV_SANDBOX_RUNTIME` picks the runner:

- `mock` (default): deterministic, dependency-free; the orchestrator
  responds to `command.request` with canned output. Used in tests and in
  this remote-execution environment where no Docker daemon is available.
- `docker`: real one-shot containers via the Docker engine API. Only works
  in binaries built with the `docker` build tag.
- `none`: no runner attached; `command.request` returns
  `error{not_implemented}`. Useful when you want the orchestrator running
  but no tool dispatch.

## Build tags

The `internal/sandbox/docker.go` file and the `factory_docker.go` resolver
are behind `//go:build docker`. The default build of `cmd/orchestrator` and
`cmd/sandbox` is Docker-unaware: they compile with no external runtime
dependencies and ship with the mock runner only.

```sh
# Default — mock runner, no Docker SDK linked.
make build

# Docker-enabled — orchestrator and sandbox CLI gain the real runner.
make build-docker
```

If you set `NOMADDEV_SANDBOX_RUNTIME=docker` against a default-build binary,
the orchestrator exits at startup with
`sandbox: docker runtime requested but binary built without -tags docker`.

## Running the Docker tests locally

```sh
make test-docker
# or equivalently:
go test -tags docker -count=1 ./internal/sandbox/...
```

Each Docker test calls `requireDaemon(t)` first; if the daemon is not
reachable the test is `t.Skip`ed rather than failed. The `ubuntu-latest`
GitHub Actions runner has Docker pre-installed, so the `test-docker` job in
[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) actually exercises
all six Docker round-trip tests on every PR (~10s total wall-clock).
`make test-docker` runs the same suite locally against your daemon.

## Future work (out of scope for Phase 3)

- Long-lived container reuse for fast-path script execution — Phase 4 may
  promote `ContainerExec*` over the current `ContainerCreate-per-request`
  model once we have tool semantics that benefit from a warm sandbox.
  `search_syntax` is the first tool that would benefit (it pays the
  full container-spinup cost for what's effectively a sub-second `sg`
  invocation), but the threat model change of running multiple tool
  calls in the same container needs its own design pass.
