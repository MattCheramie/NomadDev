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
- **AutoRemove on exit** plus a `defer ContainerRemove(force: true)` safety
  net in case `ContainerStart` fails before AutoRemove engages.
- **Single tool, single shape**. Phase 3 only accepts
  `tool="execute_script"` with `args.script` (and optional `args.shell`).
  Anything else returns `sandbox_bad_request` immediately without touching
  the engine.

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
| `NOMADDEV_SANDBOX_IMAGE` | `alpine:3.20` | container image |
| `NOMADDEV_SANDBOX_WORKSPACE_DIR` | `/var/lib/nomaddev/work` | host path bind-mounted at `/work` |

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
reachable the test is `t.Skip`ed rather than failed. CI does not run these
tests — the GitHub Actions runner has no daemon and the cost of provisioning
one for every PR is not worth the marginal coverage. Run them on your local
machine before merging anything that touches `internal/sandbox/docker.go`.

## Future work (out of scope for Phase 3)

- Additional tools (`read_file`, `write_patch`) — wait for Phase 4 alongside
  the Gemini function-call schemas.
- Long-lived container reuse for fast-path script execution — Phase 4 may
  promote `ContainerExec*` over the current `ContainerCreate-per-request`
  model once we have tool semantics that benefit from a warm sandbox.
- Per-session workspace isolation — currently the same `WorkspaceDir` is
  bound for every request; per-SID subdirectories arrive when multi-user
  support lands.
