# ⚡ NomadDev

NomadDev is an experimental, mobile-first remote execution environment. It provides a secure, natural-language-driven interface for managing remote servers, testing code, and orchestrating containers from your phone without exposing an SSH port or relying on messy terminal emulators.

By combining mesh networking, ephemeral container sandboxing, and LLM-driven RPC mapping, NomadDev allows you to interact with a headless VPS daemon securely and seamlessly.

## 🏗️ Architecture

The system is built on a "local-first" philosophy extended to remote infrastructure. Data and execution remain strictly within your private mesh network. 

The architecture is divided into six modular, decoupled components:

1. **The Secure Mesh (Connectivity):** A Tailscale overlay network ensuring the remote host and mobile client communicate exclusively over a private IP range.
2. **The Orchestrator Daemon (Backend):** A lightweight, concurrent WebSocket server written in Go that acts as the central nervous system, handling secure client connections and job routing.
3. **The Ephemeral Sandbox (Worker):** A Go-based wrapper around the Docker SDK that runs each tool call in a one-shot container with no network, read-only rootfs, and gVisor (`runsc`) isolation when the host advertises it. Hard memory / CPU / pids caps and a wall-clock timeout bound every execution.
4. **The NLP-to-RPC Middleware (Logic):** A translation layer that utilizes the Google GenAI SDK to map natural language requests to predefined JSON schemas and remote procedure calls (RPC).
5. **The GitHub MCP Backend (Integration):** A subprocess-managed embedding of the official [github-mcp-server](https://github.com/github/github-mcp-server) exposing ~75 GitHub operations as additional tool calls. Mutating operations flow through the same approval gate as shell scripts.
6. **The Control Hub (Client):** A React Native mobile application that consumes JSON event streams to render a clean, native UI instead of raw terminal output.

---

## 🗺️ Project Roadmap

### Phase 1: Mesh & Foundation — done
*Objective: Establish secure, passwordless communication between devices.*
- [x] Configure host VPS with Ubuntu 24.04.
- [x] Install and configure Tailscale subnet routing.
- [x] Verify ICMP and basic TCP packet transmission exclusively over the Tailscale IP range.
- [x] Disable public SSH access on the host (port 22).

Provisioning lives at [`infra/`](./infra/). The flow is documented end-to-end
in [`infra/RUNBOOK.md`](./infra/RUNBOOK.md): walk through
[`infra/scripts/provision.sh`](./infra/scripts/provision.sh) on a fresh
host, run [`infra/scripts/tailscale-verify.sh`](./infra/scripts/tailscale-verify.sh)
to confirm the mesh, then
[`infra/scripts/ssh-lockdown.sh`](./infra/scripts/ssh-lockdown.sh) to close
the public interface. [`infra/scripts/smoke.sh`](./infra/scripts/smoke.sh)
drives a JWT-authed `command.request` round-trip and exits non-zero on any
regression — point it at `100.x.y.z:8080` to verify the live deploy.

### Phase 2: Headless Orchestrator (Go) — done
*Objective: Build the core message relay system.*
- [x] Initialize the Go module and set up a basic TCP listener.
- [x] Implement a WebSocket server utilizing `gorilla/websocket`.
- [x] Create a standard JSON event structure for inbound/outbound payloads.
- [x] Implement JWT-based authentication to reject unauthorized WebSocket connections.
- [x] Build a robust logging and state-recovery mechanism for dropped connections.

Implementation lives under [`cmd/orchestrator`](./cmd/orchestrator/) and
[`internal/`](./internal/). See [`docs/architecture.md`](./docs/architecture.md),
[`docs/events.md`](./docs/events.md), and [`docs/auth.md`](./docs/auth.md).

### Phase 3: Ephemeral Sandbox Runner — done
*Objective: Safely execute commands and capture outputs without risking the host system.*
- [x] Integrate the official Docker SDK for Go.
- [x] Create a function to dynamically pull and spin up lightweight worker images (e.g., Alpine or Ubuntu).
- [x] Implement secure volume bind-mounts for a designated workspace directory.
- [x] Build an execution loop that runs `bash` commands inside the container and streams `stdout`/`stderr` back to the Orchestrator via channels.
- [x] Implement hard timeouts and resource limits (RAM/CPU) for the sandbox.

Runner implementation lives at [`internal/sandbox/`](./internal/sandbox/);
the orchestrator wires it in at [`internal/wsserver/sandbox.go`](./internal/wsserver/sandbox.go).
See [`docs/sandbox.md`](./docs/sandbox.md) for the architecture, threat model,
and how to switch between the mock and Docker runners.

### Phase 4: NLP Function Middleware — done
*Objective: Standardize natural language into actionable system commands.*
- [x] Integrate the Gemini API via Google AI Studio.
- [x] Define JSON schemas for core system tools (e.g., `execute_script`, `read_file`, `write_patch`).
- [x] Build the loop that receives user intent, queries the LLM, and captures the resulting Function Call.
- [x] Map the generated Function Calls directly to the Go Sandbox Runner from Phase 3.
- [x] Format execution results back into JSON for the LLM to interpret.

Translator + dispatcher + approval gate live at
[`internal/middleware/`](./internal/middleware/); filesystem-only tools live
at [`internal/fsops/`](./internal/fsops/); per-session conversation memory at
[`internal/history/`](./internal/history/). See
[`docs/middleware.md`](./docs/middleware.md) for the full architecture and
[`docs/approval.md`](./docs/approval.md) for the human-in-the-loop state
machine.

### Phase 5: Mobile Control Hub — done
*Objective: Ditch the terminal for a native, reactive mobile interface.*
- [x] Scaffold a new React Native (or Expo) project.
- [x] Implement a WebSocket client that connects to the Orchestrator's Tailscale IP.
- [x] Build the main chat/event feed UI components.
- [x] Create custom UI cards for "Action Approvals" (intercepting sensitive commands before they run).
- [x] Implement background synchronization to fetch state history upon app resume.

Expo + TypeScript SPA at [`mobile/`](./mobile/), exported as static web
assets and embedded into the orchestrator binary via
[`internal/wsserver/spa.go`](./internal/wsserver/spa.go). The same
Tailscale IP that exposes `/ws` also serves the UI at `/`. Three routes
(`/onboard`, `/chat`, `/settings`) over
`@react-navigation/native-stack`. JWT onboarding ships as a QR helper at
[`scripts/qr-jwt/`](./scripts/qr-jwt/). See
[`docs/mobile.md`](./docs/mobile.md) for the architecture and
[`docs/auth.md`](./docs/auth.md) for the onboarding flow.

### Phase 6: Production Readiness — done
*Objective: Take the stack from feature-complete to operable on real hosts.*
- [x] Persistent session replay buffer (SQLite write-through, rehydrates on restart).
- [x] Prometheus `/metrics` endpoint covering WS, replay, sandbox, and middleware turns.
- [x] Multi-stage `Dockerfile` (distroless/static, pure-Go SQLite, no cgo) + `docker-compose.yml`.
- [x] Hardened systemd unit + non-destructive installer script.
- [x] Mobile offline outbox + interactive Settings (Reset history, Force reconnect).
- [x] Tag-driven release workflow → binaries + multi-arch GHCR image.

mTLS / per-cert subject mapping is an explicit non-goal for this round —
the Tailscale tailnet already gates network reachability, and JWT
remains the single auth source for `/ws`.
[`docs/operations.md`](./docs/operations.md) is the operator reference;
[`infra/RUNBOOK.md`](./infra/RUNBOOK.md) is the deploy walkthrough.

### Phase 7: GitHub MCP Integration — done
*Objective: Let the mobile chat drive GitHub (issues, PRs, repos, …) the
same way it drives shell scripts and files, with the same approval gate.*
- [x] Subprocess-based MCP client embedding the official
  [github-mcp-server](https://github.com/github/github-mcp-server) — no
  exposure to its "Go API is unstable" warning.
- [x] All ~75 tools across 19 toolsets exposed to Gemini via the existing
  function-calling loop; tool list narrowable via `NOMADDEV_GITHUB_TOOLSETS`.
- [x] Auto-approval gating: every tool the upstream marks
  `DestructiveHint=true` (with a verb-prefix fallback) is added to the
  required-approval set at startup. PRs, issues, file writes all surface the
  same `ApprovalSheet` the mobile UI already renders for shell scripts.
- [x] `TokenSource` interface keeps per-user PAT / GitHub App / OAuth as
  drop-in future implementations.
- [x] Build-tag-gated (`-tags github`) so default builds stay slim;
  `NOMADDEV_GITHUB_TOKEN` empty is a silent no-op for development.
- [x] `nomaddev_github_calls_total{tool,outcome}` counter for per-tool
  observability.
- [x] Mobile `ApprovalSheet` surfaces a **GITHUB** badge for `github_*`
  tools so operators instantly distinguish remote-state approvals from
  local sandbox/fsops ones.
- [x] Opt-in live round-trip test (`make test-github-live`) that drives
  the real upstream binary; CI skips silently when the PAT env var and
  binary aren't present.
- [x] Production deploy paths: GHCR Docker image bundles a pinned
  `github-mcp-server` so `docker compose up` works with no extra
  install; release-workflow binaries built with `-tags "gemini github"`
  so `.tar.gz` downloads from the releases page have the integration
  compiled in.
- [x] Per-call timeout honored: `DispatchOptions.Timeout` caps the
  upstream MCP round-trip so a hung GitHub request surfaces as
  `SandboxErrTimeout` instead of hanging the turn.
- [x] Subprocess supervision: a crashed `github-mcp-server` is detected
  on the next tool call, respawned, and the call retried once.
  Cooldown-throttled (5 s minimum between attempts) so a flapping
  upstream binary can't loop.
- [x] Latency histogram (`nomaddev_github_call_seconds`) for SLO
  dashboards; bad-args / approval-denied pre-flights are excluded so
  the histogram tracks only real upstream round-trips.
- [x] `quickstart-systemd.sh` auto-installs `github-mcp-server` when
  `NOMADDEV_GITHUB_TOKEN` is configured — single-command deploy for
  the systemd path matches the Docker path.
- [x] Pre-flight argument size cap (`NOMADDEV_GITHUB_MAX_ARG_BYTES`,
  default 256 KiB) — an LLM emitting a 100 MB blob is rejected as
  `SandboxErrBadRequest` before the stdio pipe sees it.
- [x] Sensitive-arg redaction in the `command.request` /
  `tool.approval.request` wire envelopes — values for keys matching
  `token` / `password` / `secret` / `auth` / `api_key` /
  `credential` / etc. are masked on the wire (display only; dispatch
  still gets the originals). Long strings truncated to 4 KiB.
- [x] Upstream API drift CI guard
  ([`.github/workflows/upstream-drift.yml`](./.github/workflows/upstream-drift.yml))
  runs a weekly + on-PR smoke against the latest `github-mcp-server`
  release so breaking changes surface before we bump the pinned
  version in the Dockerfile.

See [`docs/github.md`](./docs/github.md) for setup, PAT scopes,
troubleshooting, and the auth-extension seam.

---

## 🚀 Running the orchestrator

```sh
export NOMADDEV_JWT_SECRET="$(head -c 48 /dev/urandom | base64 | tr -d '\n')"
make build
./bin/orchestrator -listen :8080
```

In another shell, mint a token and connect:

```sh
TOKEN="$(go run ./scripts/gen-jwt -sub matt -sid sess-1 -ttl 1h)"
./bin/wsclient -url ws://127.0.0.1:8080/ws -token "$TOKEN" -send ping
```

Drive the Phase 3 sandbox runner end-to-end against the mock backend:

```sh
./bin/wsclient -url ws://127.0.0.1:8080/ws -token "$TOKEN" \
  -send command.request -script 'echo hi' \
  -disconnect-after command.result -timeout 5s
```

Drive the Phase 4 NLP middleware turn loop with the mock translator and the
auto-grant approval bypass (memory history so it doesn't touch `/var/lib`):

```sh
export NOMADDEV_MIDDLEWARE_RUNTIME=mock
export NOMADDEV_APPROVAL_AUTO_GRANT=true
export NOMADDEV_HISTORY_BACKEND=memory
./bin/orchestrator -listen :8080 &
./bin/wsclient -url ws://127.0.0.1:8080/ws -token "$TOKEN" \
  -send user.intent -text "hello there" \
  -disconnect-after assistant.message -timeout 10s
```

Build the Phase 5 SPA into the orchestrator binary and connect with a
browser:

```sh
make build-full              # npm install + expo export → embed → go build
./bin/orchestrator -listen :8080 &
go run ./scripts/qr-jwt \
    -server-url http://127.0.0.1:8080 -sub matt -sid sess-1 -ttl 1h \
    -out qr.png
# stdout prints the deep-link URL — open it in a browser or scan qr.png.
```

For SPA dev with Metro hot-reload, run `make dev-mobile` and point the
Expo dev server at the orchestrator (Expo serves the UI on its own port;
the WebSocket connects back to `:8080/ws`).

Run the test suite:

```sh
make test-race          # default Go suite — mock sandbox + mock translator
make test-mobile        # mobile SPA tests (Jest + mock-socket)
make test-docker        # real Docker runner round-trip (requires daemon)
make test-gemini        # real Gemini API (requires NOMADDEV_GEMINI_API_KEY)
```

CI exercises the default Go suite, the SPA test suite (Jest), the
Docker-tagged sandbox tests (the `ubuntu-latest` runner has Docker
pre-installed), and tag-build smoke covering `-tags docker`, `-tags
gemini`, and the combined build. See
[`.github/workflows/ci.yml`](./.github/workflows/ci.yml).

The Docker-tagged tests (`internal/sandbox/docker_test.go`) call
`requireDaemon(t)` and skip cleanly on machines without a daemon. The
Gemini-tagged tests (`internal/middleware/gemini_test.go`) call
`requireKey(t)` and skip when `NOMADDEV_GEMINI_API_KEY` is absent. Build the
Docker-enabled binaries with `make build-docker`, the Gemini-enabled binaries
with `make build-gemini`, or both with `make build-all`. See
[`.env.example`](./.env.example) for all configuration knobs.

---

## 🚢 Deploying

**Prerequisites:** A fresh Ubuntu VPS (any provider — verified on Hetzner
CX22 / CAX11), a Tailscale account. No DNS, no certificate, no extra
infrastructure.

Pick one path:

| Path | When to use | One-command deploy |
| --- | --- | --- |
| **Docker / GHCR** | Default. Sidesteps Go 1.25 / npm build on the VPS by pulling the prebuilt multi-arch image. | `sudo bash infra/scripts/quickstart-docker.sh` |
| **systemd** | When you don't want Docker on the box. Downloads the matching prebuilt binary from the latest GitHub release. | `sudo bash infra/scripts/quickstart-systemd.sh` |

Both quickstarts auto-detect the tailnet IPv4, generate `NOMADDEV_JWT_SECRET`,
install/start the service, and run the smoke test. Re-runnable.

See [`infra/RUNBOOK.md`](./infra/RUNBOOK.md) for the full manual
walkthrough (review-every-script discipline), Hetzner-specific notes
(Cloud Firewall, CX22 sizing, IPv6), and incident response. The Docker
image is built from the multi-stage [`Dockerfile`](./Dockerfile)
(distroless/static, pure-Go SQLite, `CGO_ENABLED=0`); the systemd unit
at [`infra/systemd/nomaddev-orchestrator.service`](./infra/systemd/nomaddev-orchestrator.service)
runs as a dedicated `nomaddev` user with `NoNewPrivileges`,
`ProtectSystem=strict`, and `ReadWritePaths=/var/lib/nomaddev`.

Metrics: the orchestrator exposes Prometheus instruments at `/metrics`
(connection counts, replay events, sandbox-run histograms, middleware turn
histograms). Scrape from a Prometheus instance on the tailnet.

---

## 🛡️ Security Considerations

NomadDev is designed with paranoia as a feature. The public internet never touches the orchestrator. The LLM never touches the host system. The client never touches raw SSH.

*   **No Open Ports:** Bypasses traditional firewall risks via Tailscale.
*   **Total Isolation:** Execution occurs entirely within ephemeral Docker containers.
*   **Human-in-the-Loop:** Destructive commands parsed by the middleware require explicit biometric approval on the mobile client.

### Networking and TLS

**No SSL/TLS certificate is required to run NomadDev.** The orchestrator
listens on plain HTTP (`:8080`) by design — Tailscale already encrypts
every byte between the host and the client device, and the JWT gates
`/ws`. There is no HSTS, no `http→https` redirect, and no cert manager
in the stack. The mobile SPA does not use any secure-context-only
browser APIs (`crypto.subtle`, service workers, etc.); the only crypto
call is `crypto.getRandomValues`, which works on plain HTTP.

If your organization demands HTTPS, drop Caddy or nginx in front of
`:8080` on the tailnet and point QR onboarding at the proxy URL. The
WS client adapts `http://` → `ws://` and `https://` → `wss://`
automatically. See [`docs/auth.md`](./docs/auth.md#tls-termination) for
details. Adding TLS support to the orchestrator binary itself is an
explicit non-goal.
