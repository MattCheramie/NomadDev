# ⚡ NomadDev

NomadDev is an experimental, mobile-first remote execution environment. It provides a secure, natural-language-driven interface for managing remote servers, testing code, and orchestrating containers from your phone without exposing an SSH port or relying on messy terminal emulators.

By combining mesh networking, ephemeral container sandboxing, and LLM-driven RPC mapping, NomadDev allows you to interact with a headless VPS daemon securely and seamlessly.

## 🏗️ Architecture

The system is built on a "local-first" philosophy extended to remote infrastructure. Data and execution remain strictly within your private mesh network. 

The architecture is divided into six modular, decoupled components:

1. **The Secure Mesh (Connectivity):** A Tailscale overlay network ensuring the remote host and mobile client communicate exclusively over a private IP range.
2. **The Orchestrator Daemon (Backend):** A lightweight, concurrent WebSocket server written in Go that acts as the central nervous system, handling secure client connections and job routing.
3. **The Ephemeral Sandbox (Worker):** A Go-based wrapper around the Docker SDK that runs each tool call in a one-shot container with no network, read-only rootfs, and gVisor (`runsc`) isolation when the host advertises it. Hard memory / CPU / pids caps and a wall-clock timeout bound every execution.
4. **The NLP-to-RPC Middleware (Logic):** A translation layer that maps natural language requests to predefined JSON schemas and remote procedure calls (RPC). Pluggable provider backends: Google GenAI (Gemini), OpenAI Chat Completions, Anthropic Messages API, and DeepSeek — each selectable via the `NOMADDEV_MIDDLEWARE_RUNTIME` env var and gated behind its own build tag.
5. **The GitHub MCP Backend (Integration):** A subprocess-managed embedding of the official [github-mcp-server](https://github.com/github/github-mcp-server) exposing ~75 GitHub operations as additional tool calls. Mutating operations flow through the same approval gate as shell scripts.
6. **The Control Hub (Client):** Two interchangeable clients consume the same WebSocket protocol. The React Native + Expo SPA (Phase 5) is exported as static web assets and embedded into the orchestrator binary — it works in any phone browser without an install step. The native Go app (Phase 16, Android-first) is a single Gio binary sideloaded onto the device, with platform-native keystores, image pickers, and lifecycle integration.

---

## 🗺️ Project Roadmap

### Phase 1: Mesh & Foundation — done
*Objective: Establish secure, passwordless communication between devices.*
- [x] Configure host VPS with Ubuntu 24.04.
- [x] Install and configure Tailscale subnet routing.
- [x] Verify ICMP and basic TCP packet transmission exclusively over the Tailscale IP range.
- [x] Disable public SSH access on the host (port 22).

**New here? Start with [`docs/operator-guide.md`](./docs/operator-guide.md)** —
the single linear path from a bare VPS to a running, phone-connected
orchestrator (provision → deploy → verify → onboard → operate).

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
- [x] Define JSON schemas for core system tools (e.g., `execute_script`, `read_file`, `write_patch`, `apply_code_patch`, `search_syntax`).
- [x] Persistent reference buffer: `pin_file` / `unpin_file` tools store raw file contents in an in-memory, per-session map in `internal/history` — kept out of the event log so the summarization compactor can't drop them — and `LoadWindow`'s caller injects them at the top of the system prompt every turn, keeping critical architectural files in context through long execution chains.
- [x] Build the loop that receives user intent, queries the LLM, and captures the resulting Function Call.
- [x] Map the generated Function Calls directly to the Go Sandbox Runner from Phase 3.
- [x] Format execution results back into JSON for the LLM to interpret.
- [x] Audit / dry-run mode: `user.intent` envelopes may carry `mode: "audit"`. The orchestrator strips `execute_script`, `write_patch`, `apply_code_patch`, and destructive `github_*` tools from the catalogue before the schema reaches Gemini, and the dispatcher refuses to run them defense-in-depth. The assistant is steered to produce a markdown report.
- [x] External documentation fetcher: the `fetch_external_docs` tool lets the model re-check an external API's current schema when a remote script is failing against it. The Go backend ([`internal/docfetch`](./internal/docfetch/)) issues a single hardened GET — only `http`/`https`, with an SSRF guard that inspects the resolved IP of every connection (including redirect hops) and refuses private, loopback, link-local and carrier-grade-NAT addresses — strips the HTML/CSS/script/navigation chrome down to markdown text via `golang.org/x/net/html`, and returns the payload to the orchestrator. A strict 10-second timeout and a 2 MB response cap prevent hangs. Read-only, so it needs no approval and stays available in audit mode.
- [x] Exfiltration screen for `fetch_external_docs`: rather than gate the tool behind approval, the backend refuses a *crafted URL* before any request leaves the host. It rejects credentials in the userinfo component, values matching a known secret format (AWS/GitHub/Google/Slack/OpenAI keys, JWTs, PEM private keys), query parameters named like a credential, and high-entropy base64/hex tokens smuggled into the path, query or fragment. Operators can additionally pin the tool to an allowlist of documentation domains (and their subdomains) with `NOMADDEV_DOC_FETCH_ALLOWED_DOMAINS`, enforced on every redirect hop.
- [x] Multi-provider LLM support: alongside Gemini, the middleware ships
  drop-in `Translator` implementations for **OpenAI Chat Completions**
  ([`internal/middleware/openai.go`](./internal/middleware/openai.go)),
  the **Anthropic Messages API**
  ([`internal/middleware/anthropic.go`](./internal/middleware/anthropic.go)),
  and **DeepSeek** (reuses the OpenAI client with the DeepSeek base URL
  pre-filled by the factory, since DeepSeek's API is OpenAI-compatible).
  Each provider is gated behind its own build tag (`-tags openai`,
  `-tags anthropic`) so the default orchestrator binary stays SDK-free.
  Operators select a backend with
  `NOMADDEV_MIDDLEWARE_RUNTIME=mock|gemini|openai|anthropic|deepseek|none`
  and supply per-provider credentials via `NOMADDEV_{OPENAI,ANTHROPIC,
  DEEPSEEK}_API_KEY` (plus optional `_MODEL` overrides and
  `NOMADDEV_OPENAI_BASE_URL` for Azure / proxy deployments). See
  [`internal/middleware/README.md`](./internal/middleware/README.md) for
  the build matrix.
- [x] Per-LLM transport-level retry budget configurable via
  `NOMADDEV_LLM_MAX_RETRIES` (default 2). OpenAI and Anthropic SDKs both
  back off exponentially on 408/409/429/5xx responses; Gemini's policy is
  hardcoded by the upstream SDK and not overridable. Sandbox / tool-call
  retries continue to flow through the separate `NOMADDEV_MAX_AUTORETRIES`
  recovery budget at the dispatch layer.
- [x] Cost accounting in USD: a hard-coded per-`(provider, model)` price
  table at [`internal/middleware/pricing/`](./internal/middleware/pricing/)
  derives a `nomaddev_llm_cost_usd_total` Prometheus counter (labeled by
  provider + model) alongside the existing `nomaddev_llm_tokens_total`
  (which now also carries `provider` + `model` labels). The terminal
  `assistant.message.usage` envelope shipped to the Mobile Control Hub
  carries an additional `cost_usd` field so the per-session 'Session
  Cost' ticker can render real dollars instead of just tokens.
- [x] Anthropic extended thinking surfaced as a distinct
  [`assistant.thinking`](./docs/events.md) wire envelope and
  `AssistantEvent.Thinking` field. Enable per-deploy with
  `NOMADDEV_ANTHROPIC_THINKING_BUDGET` (`>=1024`); other backends ignore
  it. Thinking frames stream alongside (not within) the regular
  `assistant.chunk` stream so clients can render the model's reasoning
  separately from its final answer.
- [x] Multimodal (image) inputs on `user.intent` envelopes. The mobile
  Composer has an attachment button backed by `expo-image-picker`; picked
  images are base64-encoded and sent in the envelope's `images` field
  (an array of `{media_type, data}` blocks). The orchestrator validates
  count + decoded size against `NOMADDEV_USER_INTENT_MAX_IMAGES` (default
  4) and `NOMADDEV_USER_INTENT_MAX_IMAGE_BYTES` (default 5 MiB), then
  forwards to whichever backend is active: Gemini wraps each image as an
  `InlineData` part, Anthropic as an `ImageBlock`, OpenAI as an
  `image_url` content part (DeepSeek inherits the OpenAI path). Images
  are persisted in `history.Turn.Parts` so subsequent turns in the same
  session can refer back to them. Allowed media types are restricted to
  `image/jpeg`, `image/png`, `image/gif`, and `image/webp` (the
  intersection of the three providers' supported sets).
  - **Vision-capable models**: every provider's defaults
    (`gpt-4o-mini`, `claude-sonnet-4-5`, `gemini-2.0-flash`) accept
    images. DeepSeek's `deepseek-chat` and `deepseek-reasoner` are
    text-only; pair the DeepSeek runtime with `NOMADDEV_DEEPSEEK_MODEL=deepseek-vl2`
    to use vision. OpenAI's `o3-mini` is also text-only.
  - **Guardrail**: image-bearing `user.intent` envelopes are rejected
    up-front with a `bad_envelope` error when the active runtime+model
    is known text-only (see `pricing.SupportsVision`), so the operator
    sees a clear "switch to deepseek-vl2" diagnostic instead of an
    opaque upstream-provider 4xx. Unknown models pass through —
    upstream surfaces any model-specific error.
- [x] Runtime model switching from the mobile UI. The `hello` envelope
  advertises the active `provider`, the current `model`, and the
  provider's `available_models` catalogue (from
  [`middleware.KnownModels()`](./internal/middleware/models.go)); the
  mobile Settings screen renders a picker from it. Tapping a row sends a
  `user.command{action:"set_model"}` envelope — the orchestrator
  validates the model against the active provider's catalogue, stores a
  per-session (per-SID) override, and the next `user.intent` picks it up
  via `TurnInput.Model`. `reset_history` clears the override; the client
  re-applies its remembered choice on reconnect by reading
  `hello.model`. Switching the *provider itself* (e.g. openai →
  anthropic) stays a startup-only knob — it needs different credentials
  and a different build tag.

Translator + dispatcher + approval gate live at
[`internal/middleware/`](./internal/middleware/); filesystem-only tools live
at [`internal/fsops/`](./internal/fsops/); per-session conversation memory at
[`internal/history/`](./internal/history/). See
[`docs/middleware.md`](./docs/middleware.md) for the full architecture and
[`docs/approval.md`](./docs/approval.md) for the human-in-the-loop state
machine.

`search_syntax` shells out to [ast-grep](https://ast-grep.github.io/) (`sg`)
inside the sandbox worker so the model can run structural AST queries
(e.g. `fn $F($_: context.Context)`) instead of authoring fragile regex. The
binary is pre-baked into the dedicated sandbox image built from the
`sandbox` Dockerfile target:

```
docker build --target sandbox -t nomaddev/sandbox:bookworm-sg .
NOMADDEV_SANDBOX_IMAGE=nomaddev/sandbox:bookworm-sg ./orchestrator
```

The envelope returned to the model is capped by the same
`NOMADDEV_GITHUB_MAX_RESULT_BYTES` (default 1 MiB) that gates GitHub MCP
results, so a permissive pattern can't blow the context window.

### Phase 5: Mobile Control Hub — done
*Objective: Ditch the terminal for a native, reactive mobile interface.*
- [x] Scaffold a new React Native (or Expo) project.
- [x] Implement a WebSocket client that connects to the Orchestrator's Tailscale IP.
- [x] Build the main chat/event feed UI components.
- [x] Create custom UI cards for "Action Approvals" (intercepting sensitive commands before they run).
- [x] Implement background synchronization to fetch state history upon app resume.
- [x] Live Terminal inside each Action Card — virtualised, auto-tailing
  view of streamed `command.chunk` output with a heartbeat-driven
  elapsed-time indicator (`sandbox.heartbeat`) so the operator can see
  long-running jobs are still alive between bursts of output.

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
- [x] Prometheus `/metrics` endpoint covering WS, replay, sandbox, middleware turns, and LLM token usage.
- [x] Multi-stage `Dockerfile` (distroless/static, pure-Go SQLite, no cgo) + `docker-compose.yml`.
- [x] Hardened systemd unit + non-destructive installer script.
- [x] Mobile offline outbox + interactive Settings (Reset history, Force reconnect, Model picker).
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
- [x] Result size cap (`NOMADDEV_GITHUB_MAX_RESULT_BYTES`, default
  1 MiB): a `get_file_contents` returning a 50 MB blob is replaced
  with a preview-bearing truncated envelope (`truncated: true`,
  `original_bytes`, head-of-payload) so it can't blow Gemini's
  context window.
- [x] Per-user PAT routing via `NOMADDEV_GITHUB_USER_TOKENS_PATH` —
  JSON file mapping JWT `sub` → fine-grained PAT, plumbed via
  `WithUserSub(ctx, sub)` from the wsserver layer to a
  `PerUserTokenSource` that falls through to the shared default on
  miss. Hot-reload on file mtime change. The `TokenSource` interface
  remains the seam for DB-backed or OAuth-onboarded variants.
- [x] Live API CI smoke
  ([`.github/workflows/github-mcp-live.yml`](./.github/workflows/github-mcp-live.yml))
  — weekly + manual workflow that drives `TestLive_*` against the
  real GitHub API on the pinned upstream version. Secret-gated
  (`GITHUB_MCP_LIVE_TOKEN`) so forks and external PRs skip cleanly.

See [`docs/github.md`](./docs/github.md) for setup, PAT scopes,
troubleshooting, and the auth-extension seam. The GitHub MCP
integration is 100% feature-complete; future work tracks upstream
catalogue growth, not capability gaps.

### Phase 8: Security hardening — done
*Objective: Work the prioritized top-10 from the missing-features
review at `/root/.claude/plans/review-this-repository-and-delegated-moon.md`.
Each numbered subsection shipped independently as its own PR. 10/10
complete; the review's wider gap list (~50 items grouped by lens)
remains the backlog source.*

#### 8.1 Auth — access/refresh + revocation — done
*Closes the "stolen JWT is good until expiry" gap and stops forcing
mobile users to re-onboard every time their access token rolls.*
- [x] **Two token kinds.** Tokens carry a `kind` claim:
  `access` (short-lived, presented at `/ws`) or `refresh` (long-lived,
  only valid at `POST /auth/refresh`). Defaults: access `1h`, refresh
  `720h` (30 days). Tokens minted before Phase 8 (no `kind` claim) are
  accepted as `access` for back-compat.
- [x] **`POST /auth/refresh`.** Mobile clients exchange a refresh token
  for a fresh `(access, refresh)` pair. The presented refresh JTI is
  rotated into the revocation list so it can never be replayed.
  Accepts the token in the `Authorization` header, a JSON body, or a
  form field.
- [x] **`POST /auth/revoke`.** Authenticated revocation endpoint —
  the caller's own token (access or refresh) is added to the
  revocation list. Idempotent (204 either time). A leaked token can
  now be killed before it expires naturally.
- [x] **JTI revocation list with three backends:**
  `sqlite` (durable across restarts, default — file at
  `NOMADDEV_AUTH_REVOCATION_PATH`), `memory` (lost on restart),
  `none` (pre-Phase-8 behavior). A janitor goroutine prunes entries
  whose `exp` has passed.
- [x] **`gen-jwt -kind {access|refresh|pair}`** for issuing the new
  token shapes; `pair` emits both as JSON for piping into onboarding.
- [x] **`/ws` enforces `kind=access`.** Refresh tokens presented at
  `/ws` are rejected with 401 before upgrade — defense in depth
  against accidental or malicious replay.

See [`docs/auth.md`](./docs/auth.md) for the full claim shape,
endpoint contracts, and revocation backend notes.

#### 8.2 Sandbox image digest pinning — done
*Closes the supply-chain hole where a compromised registry could
repoint `alpine:3.20` at a malicious manifest between deploys.*
- [x] `NOMADDEV_SANDBOX_IMAGE` accepts a content-addressed ref
  (`alpine:3.20@sha256:…`). Docker enforces the digest at pull time;
  the runner additionally re-inspects the local image before every
  exec and refuses to start the container if `RepoDigests` no longer
  contains the expected digest — catches a host-local `docker tag`
  attack that would otherwise bypass pull verification.
- [x] `NOMADDEV_SANDBOX_REQUIRE_DIGEST=true` hard-fails at boot on a
  tag-only image so a misconfigured production deploy can't silently
  fall back to the unpinned path. Default `false` for back-compat.
- [x] Parser is shared across builds (no `-tags docker` needed for
  the validation tests) and emits a structured warning when the
  configured image is unpinned, so operators see the recommendation
  in the startup log.

See [`docs/sandbox.md`](./docs/sandbox.md#threat-model) for the
verification flow and threat-model rationale.

#### 8.3 WebSocket guards — body size cap + per-connection rate limit — done
*Closes the trivial-DoS surface where a hostile client can either send a
1 GB envelope (OOM) or stream tens of thousands of small frames a second
(starve the dispatcher) without hitting any per-server cap.*
- [x] `NOMADDEV_WS_MAX_MESSAGE_BYTES` (default 256 KiB) bounds inbound
  frame size via `gorilla/websocket`'s `SetReadLimit`. Oversized
  frames are closed with the standard 1009 (`message too big`) code
  and counted on `nomaddev_ws_inbound_rejected_total{reason="message_too_large"}`.
- [x] `NOMADDEV_WS_RATE_LIMIT` (envelopes/sec) + `NOMADDEV_WS_RATE_BURST`
  (bucket size) cap inbound envelopes per connection via a token-bucket
  limiter (`golang.org/x/time/rate`). Rejected frames return a structured
  `error{code: "rate_limited"}` envelope without dropping the connection —
  a well-behaved client can throttle and resume.
- [x] Both knobs default to permissive-but-safe values; set
  `NOMADDEV_WS_RATE_LIMIT=0` to disable rate limiting entirely.
- [x] Metric `nomaddev_ws_inbound_rejected_total{reason}` for SLO
  dashboards and abuse alerts.

#### 8.4 Supply chain — SBOM + cosign + Trivy + govulncheck — done
*Lets operators verify the binary / image they downloaded was built by
this repo on a tag push and contains no known HIGH/CRITICAL CVEs.*
- [x] **Release artifacts now ship SBOMs.** Every binary in the GitHub
  release has a matching `.spdx.json` (Syft, SPDX-JSON predicate) plus
  a `.sig` + `.pem` cosign signature pair (keyless via Sigstore Fulcio
  + Rekor). The container image is signed by digest with `cosign sign`
  and the SBOM is attached as a `cosign attest --type spdxjson`
  attestation.
- [x] **CI fails on supply-chain regressions.** `aquasecurity/trivy-action`
  scans the production Dockerfile build on every PR and fails on
  `HIGH`/`CRITICAL` CVEs in OS or Go-library layers (with
  `ignore-unfixed: true` so we don't block on unpatched upstream CVEs
  that the SBOM still surfaces downstream). `golang.org/x/vuln`'s
  `govulncheck` covers reachable vulns in the Go module graph on the
  same trigger.
- [x] **Verification is documented.** [`docs/supply-chain.md`](./docs/supply-chain.md)
  walks through `cosign verify-blob`, `cosign verify`, and
  `cosign verify-attestation` with the exact
  `--certificate-identity-regexp` operators should require.

#### 8.5 Audit log — split from replay buffer — done
*Until now the per-session replay buffer doubled as an audit trail —
fine for client reconnect, useless for "who did what when" queries
without scraping every SID's ring buffer. This carves out a dedicated
JSON-Lines sink so security tooling has one stable stream to consume.*
- [x] **New `internal/audit` package.** `Event` struct, `Sink`
  interface (`Log`, `Close`), and four backends: `none` (silent),
  `stderr` (default — interleaves with regular slog, grep by `kind`),
  `stdout` (sidecar-friendly), `file` (append-only at `0o600`,
  parent dir created at `0o700`).
- [x] **Wired into the four security-critical paths:**
  `ws.connect` (sub, sid, remote, jti), `ws.auth_failed` (remote,
  reason), `auth.refresh` and `auth.revoke` (sub, sid, jti,
  token_kind), and `approval.granted` / `approval.denied` (sub, sid,
  approval id, deny reason). Each line is self-contained JSON —
  pipe straight into `jq`, promtail, or a SIEM agent.
- [x] **Defaults to `stderr`** so operators see audit events from
  the first boot without configuring a path; flip to `file` for
  durable per-deploy logs.
- [x] **Audit calls never block or fail the action they record.**
  Write errors fall back to slog rather than propagating; the
  approval grant/deny flow proceeds whether or not the sink wrote.

See [`internal/audit/audit.go`](./internal/audit/audit.go) for the
event schema and [`internal/wsserver/audit_integration_test.go`](./internal/wsserver/audit_integration_test.go)
for the end-to-end wiring tests.

#### 8.6 Approval consent — typed confirmation gate — done
*The original README claimed "explicit biometric approval" but the
SPA shipped a one-tap Approve button. Native biometrics (Face ID /
Touch ID) are unavailable in the web-only export, and WebAuthn
requires HTTPS — which the default deploy doesn't have because
Tailscale handles transport encryption end-to-end. This phase aligns
the README with reality and adds a real explicit-consent gate that
works on the plain-HTTP deploy.*
- [x] **Typed-confirmation gate** (`ApprovalSheet`): the operator
  must type the exact tool name (case-insensitive) before the
  Approve button enables. Disabled state surfaces as
  `accessibilityState.disabled` so screen readers announce it. Deny
  remains one-tap with the existing optional reason field.
- [x] **`requireTypedConfirmation` prop** (default `true`) lets
  callers opt out (test fixtures, low-risk deployments).
- [x] **README accuracy fix.** The Security Considerations bullet
  now describes typed-confirmation as the default and points
  WebAuthn-based biometric at the TLS-reverse-proxy upgrade path.
- [x] **WebAuthn is the documented next step** for operators behind
  TLS termination; it stays out of this phase to keep scope tight
  and avoid forcing an HTTPS dependency on the default deploy.

#### 8.7 SQLite integrity check + forward-only migrations — done
*Protects existing user state from a bad upgrade. The previous code
ran `CREATE TABLE IF NOT EXISTS` and called it done — fine for a
fresh deploy, useless for catching a corrupted page mid-upgrade
or refusing to start when an operator accidentally downgrades to a
binary that doesn't know about the current schema.*
- [x] **`PRAGMA integrity_check` on every store**
  (`sessions.db`, `history.db`, the JTI revocation DB).
  Constructors refuse to boot on anything other than `ok` —
  page-level corruption that a normal query path might miss
  surfaces immediately at startup.
- [x] **Forward-only migration framework**
  ([`internal/dbutil`](./internal/dbutil/dbutil.go)). Each store
  declares a `[]dbutil.Migration` slice keyed by `Version`.
  Migrations run in their own transaction that also bumps
  `PRAGMA user_version` — a failed migration rolls back atomically
  and the same step retries on the next boot. Versions must be
  contiguous starting at 1.
- [x] **Refuse-to-boot on accidental downgrade.** If
  `user_version > max(migrations)`, the constructor returns
  `ErrSchemaTooNew` instead of silently writing to a schema it
  doesn't understand.
- [x] **Cross-package integration test** confirms every real store
  bumps `user_version` to ≥ 1 on first open and stays at the same
  version after a restart, catching the failure mode where a future
  maintainer wires a migration list but forgets to call `Migrate`.

See [`docs/operations.md`](./docs/operations.md#integrity-check--schema-migrations-phase-87)
for inspection commands and the migration authoring rules.

#### 8.8 Health probes — `/readyz` + Compose healthcheck — done
*The old `/healthz` returned 200 even when the SQLite stores were
unreachable, and `docker-compose.yml` had `healthcheck: disable: true`
because distroless/static ships no shell or `wget`. Both are fixed
here.*
- [x] **New `GET /readyz`** that probes each configured SQLite
  store (`sessions.db`, `history.db`, the JTI revocation DB) with a
  2-second per-probe budget and returns
  `200 {"status":"ok","checks":{...}}` or
  `503 {"status":"degraded","checks":{"name":"<error>","..."}}`.
- [x] **`/healthz` stays pure liveness** — always 200 if the
  process is responding. Restart loops bind to that; alerting binds
  to `/readyz`.
- [x] **`-healthcheck <url>` flag** on the orchestrator binary
  does a 3-second `GET` and exits `0` / `1` — reuses the same
  binary as its own probe client so distroless/static doesn't need a
  shell.
- [x] **`docker-compose.yml`** wires
  `HEALTHCHECK ["CMD", "/usr/local/bin/orchestrator", "-healthcheck", "http://127.0.0.1:8080/readyz"]`
  with a 30s interval, 3 retries, 15s start period. Compose flips
  the container to `unhealthy` after three consecutive failures and
  `restart: unless-stopped` bounces it.
- [x] **`PingContext(ctx)`** added to the three SQLite stores so the
  probe is a cheap `SELECT 1` round-trip, not a write.

See [`docs/operations.md`](./docs/operations.md#liveness-vs-readiness-phase-88)
for the liveness-vs-readiness contract and the systemd notes.

#### 8.9 GitHub MCP rate-limit awareness + bounded retry — done
*Until now, a primary or secondary GitHub rate-limit during a
github_* tool call surfaced straight to the model mid-turn — the
biggest source of "your assistant just died" failures under any
serious workload.*
- [x] **Pattern-matches the upstream's error text** (`api rate
  limit exceeded`, `secondary rate limit`, `abuse detection`,
  `rate limit reset at`, …) — the `github-mcp-server` subprocess
  can't pass headers through stdio, so the marker scan is the only
  signal we have.
- [x] **Bounded exponential backoff with jitter** between retries
  (`NOMADDEV_GITHUB_RATE_LIMIT_BASE_BACKOFF`, default `1s`; capped
  at 30s). The upstream's `Retry-After` hint, when surfaced in the
  error text, takes precedence over the calculated value.
- [x] **`NOMADDEV_GITHUB_RATE_LIMIT_RETRIES`** caps re-invocations
  (default 3). Setting to 0 disables retry entirely (pre-8.9
  behavior — first rate-limit error surfaces to the model).
- [x] **`nomaddev_github_rate_limit_retries_total{outcome}`** —
  `outcome ∈ {retried, gave_up}`. Alert on a non-zero `gave_up`
  rate or a spike in `retried` and you know the PAT scope or tool
  mix is hitting the API too hard.
- [x] **Caller-ctx honored mid-backoff** — if the user.intent
  ctx fires while we're sleeping for a retry, we surface the
  rate-limit message immediately and bump `gave_up` rather than
  blocking past the turn budget.
- [x] **Marker-matcher and backoff helpers are tag-free** so the
  default-build suite covers them; the
  `*mcp.CallToolResult`-aware wrappers live under `-tags github`
  with their own test file.

#### 8.10 Automated SQLite backups — done
*The previous deploy mentioned `sqlite3 .backup` as a footnote and
left scheduling to the operator. Now the systemd quickstart installs
a daily backup timer; the Docker path inherits the same script via
documented host-cron usage.*
- [x] **`infra/scripts/nomaddev-backup.sh`** — uses
  `sqlite3 .backup` (online API, safe with concurrent writers) for
  each of the three SQLite stores
  (`sessions.db`, `history.db`, `revocations.db`); verifies every
  snapshot with `PRAGMA integrity_check` *before* gzipping, so a
  corrupt source DB fails the timer rather than poisoning the
  archive directory; prunes archives older than the configurable
  retention horizon.
- [x] **`nomaddev-backup.service` + `.timer`** — a `Type=oneshot`
  unit driven by a daily timer with `RandomizedDelaySec=15min` and
  `Persistent=true` (a host that was offline at 03:00 runs the
  missed backup on next boot).
- [x] **`quickstart-systemd.sh`** installs the script to
  `/usr/local/bin/nomaddev-backup`, drops the service + timer in
  place, ensures `sqlite3` is present (via `apt-get`), and enables
  the timer. The done-message surfaces the timer next-run, snapshot
  destination, and retention.
- [x] **Configurable via env vars** —
  `NOMADDEV_BACKUP_DIR` (default `${DATA_DIR}/backups`) and
  `NOMADDEV_BACKUP_RETENTION_DAYS` (default 14). Operators on
  external storage (NFS, object-store gateway) point
  `NOMADDEV_BACKUP_DIR` at the mount and the existing systemd
  hardening (`ProtectSystem=strict`, explicit `ReadWritePaths`)
  keeps the unit tight.
- [x] **Restore procedure documented** in
  [`docs/operations.md`](./docs/operations.md#automated-sqlite-backups-phase-810)
  — stop the orchestrator, decompress the chosen snapshot, swap
  files, restart. The orchestrator's startup integrity check
  (Phase 8.7) catches any inconsistency in the restored file before
  it accepts writes.

---

### Top-10 from the missing-features review: complete

All ten items from the review's `/root/.claude/plans/review-this-repository-and-delegated-moon.md`
top-10 are now shipped (8.1 through 8.10). The review's wider gap
list still has ~50 unaddressed items grouped by lens — see the plan
file for the inventory.

### Phase 9: Developer-experience lens — done
*Objective: Work the Developer Experience lens from the review's
wider gap list. Small, cohesive items that unblock contributors.
4/4 batches shipped (9.1 governance, 9.2 CI coverage + ADR +
ChatScreen test + dev-loop docs, 9.3 session-export CLI + SQLite
chaos tests, 9.4 mobile E2E).*

#### 9.1 Governance docs — done
- [x] [`SECURITY.md`](./SECURITY.md) — disclosure policy via
  GitHub Security Advisories, supported-versions matrix,
  response-timeline commitments, and a clear in/out-of-scope list.
- [x] [`CONTRIBUTING.md`](./CONTRIBUTING.md) — local-dev setup,
  build-tag matrix, commit + PR style, CI job rollup, ADR
  convention, test layout.
- [x] [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md) — Contributor
  Covenant 2.1 by reference + the reporting channel.

#### 9.2 CI coverage + ADR practice + screen test + dev-loop docs — done
- [x] **CI coverage floor.** The `test` job now emits a
  `coverprofile`, prints the func-level summary, enforces a 55%
  minimum (current measured 64%), and uploads the report as a
  14-day artifact. Floor set well below the current level so
  legitimate refactors don't bounce the build; tighten as the
  suite grows.
- [x] **ADR practice adopted.**
  [`docs/adr/0001-record-architecture-decisions.md`](./docs/adr/0001-record-architecture-decisions.md)
  codifies when a decision warrants an ADR and pins the four-section
  format (Status / Context / Decision / Consequences). Past
  decisions stay un-ADR'd; new cross-cutting ones get one.
- [x] **`ChatScreen.test.tsx`.** The mobile suite covered
  ApprovalSheet, SettingsScreen, the store, and the wire client —
  but the top-level screen that ties them together had zero
  coverage. New tests exercise empty state, turn rendering,
  Composer submit + disabled-when-not-open, the approval grant
  (with the typed-confirmation gate from 8.6) + deny paths, and
  the gear-button navigation. 7 new tests, full mobile suite at 34.
- [x] **GitHub MCP local-dev loop.** New section in
  [`docs/github.md`](./docs/github.md#local-dev-loop-no-live-pat)
  documents the no-PAT default path plus the tiered fidelity ladder
  (upstream binary install at the pinned version → fine-grained
  PAT against a throwaway repo → mock-translator orchestrator with
  auto-grant approvals → wsclient one-shot tool call). Avoids
  burning the live-CI PAT rate budget for contributor exploration.

#### 9.3 Session-export CLI + SQLite chaos tests — done
- [x] **`cmd/session-export`** — small Go binary that dumps one
  SID's data from `sessions.db` or `history.db` as JSON Lines.
  Opens the DB **read-only** so a running orchestrator isn't
  disturbed; auto-detects which store the file is via
  `sqlite_master`. 7 tests cover SID filtering, both auto-detect
  paths, the both-tables ambiguity case, and explicit-`-kind`
  override on the wrong store.
- [x] **SQLite chaos / failure-injection tests.** New
  `internal/dbutil/chaos_test.go` covers four real-world failure
  modes: bit-flip corruption (integrity_check surfaces
  `ErrIntegrityCheckFailed`), half-truncated file (integrity_check
  or first read fails), non-SQLite file at the configured path
  (Ping fails cleanly), and atomic-rollback of a partially-applied
  migration (`alpha` table must not exist + `user_version` must
  not bump).

#### 9.4 Mobile E2E via Playwright — done
*Closes the DX-lens follow-up that needed its own PR because
Playwright brings a separate test stack (real browser, full
orchestrator round-trip) from the Jest unit suite.*
- [x] **`@playwright/test` added** as a mobile devDep (chromium
  only — extra browsers add test time without catching real
  regressions for a small web SPA).
- [x] **`mobile/e2e/onboarding-to-first-turn.spec.ts`** drives the
  exact code path operators hit on a phone: fragment-based deep
  link (`#token=…&sid=…`), fragment-stripped on first paint,
  navigates to /chat, WS handshake to "open", Composer un-disables,
  user types a turn, mock translator's canned reply lands in the
  feed.
- [x] **New `mobile-e2e` CI job.** Builds the SPA + orchestrator
  with `make build-full`, starts the binary with mock translator +
  auto-grant approvals + memory backends, waits for `/healthz`,
  mints a JWT via `scripts/gen-jwt` (masked in the workflow log),
  runs Playwright. Uploads the HTML report + traces on failure for
  post-mortem.
- [x] **Jest excludes `e2e/`** via `testPathIgnorePatterns` so the
  unit-test stack doesn't trip on Playwright's node-only globals.
  Full mobile Jest suite still at **34 passing**.

**Phase 9: Developer-experience lens — done.** All four shipped
items (9.1–9.4) plus the deferred reproducible-build verification
that needs `diffoscope` for the next attempt — captured in
[`claude/dx-tooling`](#)'s revert commit, which records the
investigator-friendly diagnostic context.

### Phase 10: Security gaps not in the top-10 — done
*Objective: Work the Security-gaps-beyond-top-10 lens from the
review's wider gap list — items the original top-10 prioritization
left for follow-up because they're either narrower in blast radius
or carry more architectural weight. Both batches (10.1 + 10.2)
shipped.*

#### 10.1 Origin allowlist + CSP + JWT rotation grace + script-content redaction — done
- [x] **`CheckOrigin` allowlist.** `gorilla/websocket`'s upgrader
  previously accepted any origin unconditionally. New
  `NOMADDEV_WS_ALLOWED_ORIGINS` (CSV) populates a strict
  case-insensitive same-origin gate on `/ws`. Empty preserves the
  pre-10.1 behavior (Tailscale deploys have no meaningful browser
  origin boundary); operators behind a TLS reverse proxy turn on
  the gate without code changes. Same-origin / non-browser clients
  without an `Origin` header always pass.
- [x] **CSP + hardening headers on the SPA.** `withSecurityHeaders`
  wraps the SPA handler with `Content-Security-Policy`
  (`default-src 'self'`, `connect-src 'self' ws: wss:`,
  `frame-ancestors 'none'`), `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: strict-origin-when-cross-origin`,
  `X-Frame-Options: DENY`. The `/ws` and `/metrics` paths keep
  their existing shapes — CSP only applies to browser-context
  responses.
- [x] **JWT secret rotation grace window.** New
  `NOMADDEV_JWT_PREV_SECRETS` (CSV) lets the verifier accept tokens
  signed under previous-generation secrets while new tokens are
  signed under `NOMADDEV_JWT_SECRET`. Rotation workflow lives in
  [`docs/auth.md`](./docs/auth.md#rotation-with-a-grace-window-phase-101).
  Startup logs `orchestrator: JWT rotation grace active` when any
  prev secrets are configured.
- [x] **Inline-script secret redaction.** The Phase-7
  `RedactArgs` helper masks values of sensitive-keyed args but
  left `script` content alone — an `export TOKEN=abc123` line in
  a bash script reached the approval card in plain text. New
  `redactScript` scans script-shaped arg values for
  `(export|set)? NAME=VALUE` shapes and masks the value when
  `NAME` matches the same sensitive-key list. Heuristic on
  purpose: prose-shaped fields (`body`, `description`) don't get
  the scanner; `script` / `command` keys do.

#### 10.2 Per-session workspace isolation + ops docs for userns + quota — done
- [x] **Per-session sandbox workspace.** New
  `NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE` flag (default false for
  back-compat). When true, the docker runner bind-mounts
  `<WorkspaceDir>/<sanitized-sid>/` at `/work` instead of the
  shared root. `sandbox.ExecRequest.SessionID` carries the SID
  from the WS layer through both the direct command.request path
  and the middleware tool-dispatcher path. The SID is sanitized
  (alphanumerics + `-_.`, capped at 64 bytes, `..` collapsed to
  `__`) so a malformed claim can't escape the workspace root.
- [x] **`sanitizeSID` tested in 4 scenarios** covering allowed
  characters, path-traversal collapse, shell-meta stripping, and
  the 64-byte length cap.
- [x] **Known limitation captured.** `fsops` still operates on the
  shared root — per-fsops isolation is a separate plumb-through
  that's deferred because the engine is a Service-level singleton
  today. Documented in
  [`docs/sandbox.md`](./docs/sandbox.md#per-session-workspace-isolation-phase-102)
  so multi-tenant operators know to treat sandbox isolation as
  defense-in-depth on top of per-user PAT scoping rather than a
  complete boundary.
- [x] **User-namespace remapping documented** in
  [`docs/sandbox.md`](./docs/sandbox.md#user-namespace-remapping-phase-10-doc).
  Daemon-level config (`/etc/docker/daemon.json` with
  `"userns-remap": "default"`); the orchestrator can't drive this
  from inside, but the doc captures the workspace-ownership
  trade-off (`chown 100000:100000` vs running orchestrator as
  `dockremap`).
- [x] **Total-resource budgeting documented** in
  [`docs/sandbox.md`](./docs/sandbox.md#total-resource-budgeting-phase-10-doc).
  Worst-case container RSS is `MAX_CONCURRENT × MEMORY`; the
  existing semaphore caps concurrent runs. Added a sizing table
  for the common deploy profiles (CX22, CAX11, multi-tenant). A
  pool-style "total memory budget" model is architecturally
  bigger than per-run caps; the per-run × concurrent product
  covers the same blast radius for any realistic deploy.

**Phase 10: Security gaps not in the top-10 — done.** Both batches
(10.1 wire + auth + redaction hardening, 10.2 per-session isolation
+ userns / quota docs) shipped. Future security follow-ups would
target per-fsops session isolation (engine refactor), a real
total-memory pool model (only if a multi-tenant deploy hits the
worst-case sizing), and per-tool scopes on the JWT.

### Phase 11: Production hardening — done
*Objective: Work the Production-hardening lens — the last remaining
lens from the missing-features review. Operator-facing observability,
deployment automation, and the docs that turn the orchestrator from
"runs on a box" into "operable in production by someone who didn't
write it."*

#### 11.1 Observability + IaC + ops docs — done
- [x] **Grafana dashboard** at
  [`monitoring/grafana-dashboard.json`](./monitoring/grafana-dashboard.json)
  — 10 panels covering the SLO surface area: active WS conns,
  connect-rate by outcome, sandbox p50/p95/p99, middleware turn
  rate + latency, per-tool GitHub MCP rate, rate-limit retries,
  inbound rejection reasons, session-event throughput by kind.
  Import via the UI (uid `nomaddev-overview`) or
  provision-as-config.
- [x] **Prometheus alert rules** at
  [`monitoring/alertmanager-rules.yml`](./monitoring/alertmanager-rules.yml)
  — 7 rules across three groups (availability, capacity,
  security). Every rule binds to a metric already exported from
  `internal/metrics`; no new instrumentation required.
- [x] **Tailscale ACL example** at
  [`infra/tailscale/acl-example.hujson`](./infra/tailscale/acl-example.hujson)
  — default-deny tailnet policy with two invariants: the
  `nomaddev-users` group reaches `:8080`, only
  `nomaddev-admins` can shell into the host. Tagged
  `tag:nomaddev-server`. Test stanzas pin the invariants so the
  admin console refuses to publish a broken policy.
- [x] **Cloud-init template** at
  [`infra/cloud-init/nomaddev-bootstrap.yaml`](./infra/cloud-init/nomaddev-bootstrap.yaml)
  — drop into a fresh Ubuntu 24.04 VPS at provision time and the
  orchestrator is up + on the tailnet without an SSH session.
  Pairs with the Tailscale ACL above. Templates JWT secret,
  Tailscale auth key, and Gemini API key from cloud-provider
  user-data substitution.
- [x] **Data-handling / privacy doc** at
  [`docs/privacy.md`](./docs/privacy.md) — inventories every
  piece of data the orchestrator touches: what's persisted, where,
  for how long, what leaves the host (Gemini, GitHub, Tailscale),
  audit-trail content, wire redaction limits, retention policy
  summary, and a wipe-everything recipe.
- [x] **Single-node disclaimer + log-rotation guidance** added to
  [`docs/operations.md`](./docs/operations.md#single-node-only-phase-11-doc).
  Captures the supported-deploy posture explicitly (no
  active-active, no failover, hub state is in-process), sketches
  what a real HA shape would need (shared DBs + stateless hub +
  network-attached audit), and ships a `/etc/logrotate.d/nomaddev`
  recipe for the file-backend audit log using `copytruncate`.

#### 11.2 OpenTelemetry tracing — done
- [x] **New `internal/tracing` package.** `Init(ctx, Config, log)`
  wires the global `TracerProvider` with an OTLP/HTTP exporter
  and returns a Shutdown hook callers defer unconditionally
  (no-op when disabled). Quiet fallback on misconfiguration —
  a typo in the OTLP URL logs a warning and disables tracing
  instead of taking the orchestrator down.
- [x] **Default off.** `NOMADDEV_OTEL_ENABLED=false` is the
  shipping default; `otel.Tracer(...)` returns a noop tracer
  at every call site so the codebase pays only the
  tens-of-nanoseconds tracer-noop cost when tracing is off.
- [x] **First span: `ws.dispatch.<envelope.type>`.** One root
  span per inbound envelope on the dispatcher entry point with
  `envelope.type`, `session.sub`, `session.sid` attributes. Gives
  operators immediate trace-side visibility per turn / per
  command.request without spreading instrumentation through
  every package; future Phase-11.3 can add child spans on
  sandbox.Exec / githubmcp.Call when the trace shape stabilizes.
- [x] **Config knobs.** `NOMADDEV_OTEL_OTLP_ENDPOINT` (collector
  URL), `NOMADDEV_OTEL_SERVICE_NAME` / `_VERSION` (resource
  attributes), `NOMADDEV_OTEL_SAMPLE_RATIO` (0.0–1.0, parent-based
  head sampling), `NOMADDEV_OTEL_INSECURE` (plain-HTTP collector
  on a Tailscale tailnet, default true). Documented in
  `.env.example` and tested in `internal/tracing/tracing_test.go`
  (disabled-default, bad-endpoint, defaults-filled-in).

#### 11.3 SIGHUP-reopen for the audit log + child spans — done
- [x] **`SIGHUP` reopens `audit.log`.** New
  `audit.Reopener` interface; `JSONSink.Reopen()` closes the
  current file and opens a fresh fd at the same path. Non-file
  sinks (`stderr` / `stdout` / `noop`) treat Reopen as a no-op
  so the SIGHUP handler in `cmd/orchestrator/main.go` calls it
  unconditionally. The logrotate recipe in
  [`docs/operations.md`](./docs/operations.md#log-rotation-phase-11-doc)
  swaps `copytruncate` for a `postrotate` SIGHUP — no events
  truncated, no in-flight buffer lost.
- [x] **`sandbox.exec` span** (Phase 11.3) on the docker runner
  with `sandbox.tool` / `sandbox.session_id` / `sandbox.shell` /
  `sandbox.timeout_ms` attributes. Wraps the bind-mount + container
  lifecycle so the span's wall-clock covers the full run.
- [x] **`github.call` span** (Phase 11.3) on the GitHub MCP
  client with `github.tool` / `github.session_id` attributes.
  Args are deliberately omitted from span attributes — they'd
  dwarf trace storage and could leak secrets.
- [x] **Two new audit tests** pin the file-Reopen path
  (write, rename, reopen, write — pre-HUP event in the rotated
  file, post-HUP event in the fresh file) and the
  non-file-sink no-op invariant.

#### 11.4 Trace-context propagation + dispatcher ctx-threading — done
- [x] **`traceparent` extraction at upgrade.** `wsHandler` calls
  `otel.GetTextMapPropagator().Extract` against the upgrade
  headers BEFORE the connection's lifetime begins; the resulting
  `connCtx` is threaded into `runConnection` → `readPump` →
  `dispatch`. A traceparent from an otel-instrumented client
  (browser SPA, curl `--header`, sibling service) lands as the
  parent of the `ws.dispatch.<envelope.type>` span.
- [x] **W3C propagator registered.** `tracing.Init` now installs a
  composite `TraceContext{} + Baggage{}` propagator — the default
  is no-op, so without this the extract call would silently lose
  every parent context.
- [x] **Dispatcher ctx threaded through to runners.**
  `handleCommandRequest` / `handleUserIntent` now take a
  `dispatchCtx` from `dispatch`; both derive their per-job
  cancel-ctx (`execCtx` / `turnCtx`) from it instead of
  `context.Background()`. The 11.3 `sandbox.exec` and
  `github.call` spans now chain under the `ws.dispatch` root
  → flame-graph view shows the full upstream → dispatch → tool
  tree end-to-end.
- [x] **New `trace_propagation_test.go`** uses the otel
  in-memory exporter to assert that a synthetic `traceparent`
  on the upgrade lands on the dispatch span's `TraceID` and
  `Parent.SpanID` — pins the contract.

**Phase 11: Production hardening — done.** Four batches shipped
(11.1 observability + IaC + privacy + ops docs, 11.2 OpenTelemetry
wiring + dispatch span, 11.3 SIGHUP-reopen + per-tool child spans,
11.4 trace propagation + dispatcher ctx threading). The
tracing story is now complete: end-to-end spans from any
otel-instrumented upstream through the orchestrator and out to
the sandbox / GitHub MCP tool.

### Phase 12: residual follow-ups — in progress

#### 12.1 Per-tool JWT scopes + query-string traceparent + reproducible-build report — done
- [x] **Per-tool JWT scopes.** New `internal/auth/scopes.go` plus
  scope checks at both dispatch entry points (the direct
  `command.request` path and the middleware tool-dispatch path).
  Two-tier policy: tokens whose `scopes` list has **no** `tools:`
  entry are **legacy-permissive** (pre-12 mints keep working);
  once any `tools:<x>` is named, strict mode kicks in and only
  listed tools are allowed. `tools:*` is the wildcard;
  `tools:github` authorizes the whole `github_*` family;
  per-tool `tools:github_<name>` always wins over the family
  scope. 7 unit tests pin the policy. Documented in
  [`docs/auth.md`](./docs/auth.md#per-tool-scopes-phase-12).
- [x] **`traceparent` via query string.** The browser
  WebSocket API doesn't let JS set custom upgrade headers, so
  the SPA can't ship a `traceparent` header. New
  `wsHandler` fallback: when the upgrade carries no
  `traceparent` header, the orchestrator extracts it from
  `?traceparent=…` on the URL instead. Header wins on both
  being present so a transparent reverse proxy can override.
  Pinned by a second propagation test using an in-memory
  exporter.
- [x] **Reproducible-build report-only CI job.** Picks up the
  PR #32 deferral. New `reproducible-build-report` job in
  `ci.yml` builds the orchestrator twice with the release-workflow
  flags, runs `diffoscope` against the two binaries when the
  hashes mismatch, and uploads the report as a 14-day artifact.
  **Non-blocking** (`continue-on-error: true`) so a real
  reproducibility regression doesn't bounce unrelated PRs —
  the artifact is the deliverable.

#### 12.2 SPA traceparent + per-fsops session isolation — done
- [x] **SPA-side `traceparent` mint + inject.** New
  `mobile/src/wire/traceparent.ts` generates a W3C
  `00-<32hex>-<16hex>-01` value per connection using
  `crypto.getRandomValues`; the WS URL builder appends it as
  `?traceparent=…`. Pairs with 12.1's server-side query-string
  fallback so mobile-side timing shares a `trace_id` with the
  server-side dispatch spans (Phase 11.2 / 11.4). 3 unit tests
  pin the W3C format, randomness, and the crypto-required
  invariant.
- [x] **Per-fsops session isolation.** Phase 10.2's known
  limitation (`fsops still operates on the unscoped root`) is
  now closed. `fsops.Engine` gains a `PerSession` field; the
  middleware dispatcher attaches the calling SID via
  `fsops.WithSessionID(ctx, sid)` before invoking
  `Engine.Run`. `resolveSafe` reads the SID from ctx and routes
  paths through `<root>/<sanitized-sid>/` (created at 0o700 on
  first use) when per-session mode is enabled. Reuses the
  Phase-10.2 `NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE` knob —
  sandbox + fsops isolate in lockstep. 4 new tests pin:
  per-SID path separation, empty-SID falls back to shared root,
  `perSession=false` ignores SID, and `..`-traversal still
  rejected under the per-SID prefix.

#### 12.3 WebAuthn — server-side ceremony + credential store — done
- [x] **New `internal/webauthn` package** wrapping
  `github.com/go-webauthn/webauthn`. `Service` owns the four
  ceremony entry points (BeginRegistration / FinishRegistration /
  BeginLogin / FinishLogin); the SQLite-backed `Store`
  persists per-(sub, credential_id) rows with the public key,
  sign count, and attestation type. Uses the Phase 8.7 dbutil
  migration pattern.
- [x] **In-memory `SessionCache`** for in-flight ceremony
  challenges. 5-minute TTL, used-once `Take` semantics so a
  replayed finish gets a clean miss; pruned on every Put / Take.
- [x] **Four new HTTP endpoints** under `/auth/webauthn/`:
  - `register/begin` + `register/finish` — JWT-gated; an
    operator must already be authenticated to add a security key
    to their `sub`.
  - `login/begin` + `login/finish` — unauthenticated; takes
    `sub` and returns a fresh JWT pair on successful assertion.
- [x] **Probe resistance.** `login/begin` returns the same
  401 message whether the sub exists with no keys or doesn't
  exist at all; the server log carries the real reason for the
  operator.
- [x] **Disabled by default.** WebAuthn requires HTTPS-or-localhost,
  which the default Tailscale plain-HTTP deploy doesn't have. The
  routes only register when `NOMADDEV_WEBAUTHN_ENABLED=true`;
  unregistered routes return 404 (the canonical "not configured"
  signal).
- [x] **9 unit tests + 5 handler tests** pin the store roundtrip,
  the session-cache TTL + used-once semantics, the disabled-route
  404, JWT-required behavior, the begin-register
  options+session-token shape, and the probe-resistant login-begin
  error.

See [`docs/webauthn.md`](./docs/webauthn.md) for the operator
workflow, threat model, and SPA-side integration sketch.

#### 12.4 WebAuthn — mobile SPA UI — done
- [x] **New `mobile/src/wire/webauthn.ts`** wraps the four server
  endpoints with a `registerSecurityKey(...)` /
  `signInWithSecurityKey(...)` pair. Owns the base64url ↔
  ArrayBuffer conversion the W3C API requires for `challenge`,
  `user.id`, `excludeCredentials[].id`, `allowCredentials[].id`,
  plus the W3C-shaped attestation / assertion JSON the server's
  go-webauthn parser expects on finish.
- [x] **Settings screen** gains a "Register security key" button
  with an optional label input. The button is gated on
  `isWebAuthnAvailable()` — present only when the page is loaded
  over HTTPS or http://localhost (matches the WebAuthn spec
  requirement and the docs/webauthn.md prerequisite).
- [x] **Onboard screen** gains a "Sign in with security key" path
  alongside the existing JWT-paste flow. On success the returned
  JWT pair lands in the same `setCredentials(url, token)` slot,
  so the WS client picks up immediately.
- [x] **Probe-resistant error passthrough.** When the server
  returns its deliberately-opaque "no security key registered for
  that account" 401, the SPA surfaces the server message verbatim
  rather than inventing a clearer "user not found" string —
  preserves the threat model end-to-end.
- [x] **16 new unit tests** (`mobile/src/__tests__/webauthn.test.ts`)
  pin the base64url roundtrip, option-decoding (creation +
  request), attestation / assertion serialization, the
  isWebAuthnAvailable feature gate, and full register / login
  ceremonies with mocked `fetch` + `navigator.credentials`.
  Browser-side `navigator.credentials.create/get` is end-to-end
  covered by Playwright's virtual authenticator when the real
  ceremony is wired into the E2E (future follow-up).

#### 12.5 History summarization compactor — done
*Closes the unbounded-growth gap in `history.db`: long-running
sessions inflated Gemini context tokens on every `user.intent`
(via `LoadWindow`) and grew the on-disk file forever. A background
goroutine now collapses the oldest half of a session's text into
one `system.summary` row once it crosses a configurable word
budget.*
- [x] **New `Compactor` + `Summarizer` in
  [`internal/history/summarizer.go`](./internal/history/summarizer.go).**
  Janitor goroutine ticks every `NOMADDEV_HISTORY_SUMMARY_INTERVAL`
  (default 5 m). For each session in `turns`, sums `strings.Fields`
  word counts across `role IN ('user','assistant')` rows; if the
  total crosses `NOMADDEV_HISTORY_SUMMARY_WORD_THRESHOLD` (default
  15000), POSTs the oldest 50 % to
  `NOMADDEV_HISTORY_SUMMARY_URL` as a `[{role,text,ts}]` array and
  reads `{"summary": "..."}` back. One transaction deletes the
  victims and inserts a single `role = 'system.summary'` row at
  the smallest freed `turn_idx` so chronological order is
  preserved. Opt-in (`NOMADDEV_HISTORY_SUMMARY_ENABLED`, default
  off); SQLite backend only.
- [x] **No schema change — Phase 8.7 contract preserved.** The
  `system.summary` value is just data in the existing
  `role TEXT` column. The migrations slice in
  [`internal/history/sqlite.go`](./internal/history/sqlite.go)
  stays at `Version: 1`. `PRAGMA user_version` is still `1`
  after the change; `internal/dbutil`'s integrity-check and
  downgrade-protection invariants are untouched.
- [x] **Concurrency-safe.** Compaction acquires the same per-SID
  mutex that `Append` uses, so `turn_idx` stays monotonic against
  concurrent wsserver appends. Tested by a 20-append /
  1-compaction race in
  [`internal/history/summarizer_test.go`](./internal/history/summarizer_test.go).
- [x] **Audit-safe.** `tool_call` / `tool_result` rows are never
  selected for summarization — the LLM-bound textual chatter goes
  through the summarizer; structured tool I/O stays intact.
- [x] **Failure-safe.** Any non-2xx response, decode error, or
  empty `summary` aborts the transaction; the database is left
  untouched and the next tick retries naturally.
- [x] **Wire-compatible.** Summary rows carry the same
  `{"text": "..."}` `parts_json` shape as user/assistant turns,
  so the translator's history-replay path needs no special-casing.
- [x] **8 new unit tests** cover below-threshold no-op,
  oldest-half replacement with tool-row preservation,
  idx-monotonic `Append` after compaction, summarizer-error
  rollback, concurrent-Append safety, reopen survival, the
  HTTP client wire shape, and multi-session sweeps.

See [`docs/middleware.md`](./docs/middleware.md#background-summarization-compactor)
for the architecture and
[`docs/operations.md`](./docs/operations.md#history-summarization-compactor)
for the env var table and inspection commands.

**Remaining Phase-12 follow-ups:** pool-style memory quota (only
if a multi-tenant deploy hits the worst-case sizing — documented
sizing approach in `docs/sandbox.md` covers the same blast
radius); mobile native build (Expo EAS — separate infra setup).

### Phase 13: Automated middleware error recovery — done
*Closes the "every failing tool call burns a human-input turn" gap:
when a middleware-dispatched `command.request` returns a retryable
failure (non-zero exit, `sandbox_timeout`, `sandbox_oom`), the
orchestrator now formats the captured stderr as a structured
`system.error_report` and feeds it back into the translation layer so
the LLM can author a fix as a new `command.request`. Bounded retry
prevents an infinite loop; final failure is escalated to the Mobile
Control Hub as a wire envelope.*

- [x] **New `system.error_report` event type** in
  [`internal/event/types.go`](./internal/event/types.go) with payload
  `{tool, original_call_id, exit_code, error_code, error_message,
  stderr, attempt, max_attempts, escalated}`. Used in two places: as a
  `ToolResult.Output["error_report"]` enrichment that the translator
  reads on the next stage, and as a wire envelope to the Mobile
  Control Hub on budget exhaustion (`escalated:true`).
- [x] **Recovery state machine** in
  [`internal/middleware/recovery.go`](./internal/middleware/recovery.go):
  `ShouldAutoRetry(exitCode, errCode)` classifies retry-eligible
  failures (non-zero exit, `sandbox_timeout`, `sandbox_oom`; structural
  errors like `sandbox_bad_request` / `sandbox_unauthorized` are
  terminal). `BuildErrorReport(...)` formats the payload and
  tail-truncates stderr to 8 KiB. `RetryBudget` tracks **consecutive**
  failures so a sporadic transient doesn't burn budget for the rest of
  a multi-step turn.
- [x] **Orchestration loop** in
  [`internal/wsserver/middleware.go`](./internal/wsserver/middleware.go).
  `consumeStage` now allocates a per-turn `RetryBudget(MaxAutoRetries)`,
  enriches the resumed `ToolResult` on retry, and on exhaustion emits
  the `system.error_report` envelope via `bufferAndSend` then closes
  the turn with `finish_reason="error"`.
- [x] **Configuration knob.** `NOMADDEV_MAX_AUTORETRIES` (default `2`)
  wires through `config.MiddlewareConfig.MaxAutoRetries` →
  `middleware.RuntimeConfig.MaxAutoRetries`. `0` disables the loop
  entirely; the first retryable failure escalates immediately.
- [x] **Test coverage.** Recovery primitives unit-tested in
  [`internal/middleware/recovery_test.go`](./internal/middleware/recovery_test.go).
  End-to-end behavior pinned by `TestMiddleware_AutoRetry_*` in
  [`internal/wsserver/middleware_test.go`](./internal/wsserver/middleware_test.go):
  single-failure recovery (no wire envelope), budget exhaustion
  (exactly one `system.error_report` envelope, three `command.request`
  envelopes for `MaxAutoRetries=2`), zero-budget immediate escalation,
  and non-retryable failures bypassing the loop.

See [`docs/middleware.md`](./docs/middleware.md#automated-error-recovery-phase-13)
for the architecture and
[`docs/events.md`](./docs/events.md#automated-error-recovery) for the
wire-level sequence diagram.

### Phase 14: `apply_code_patch` verification hook — done
*Closes the "the LLM applied a patch that breaks the build, and now the
next tool call is fighting a corrupted workspace" gap. `apply_code_patch`
gains an optional `verify_command` that runs in the ephemeral sandbox
immediately after the write; a non-zero exit rolls the file back to its
pre-edit contents and feeds the verify command's stderr into the Phase 13
auto-recovery loop so the LLM authors a fix on the next stage.*

- [x] **Schema + validation.** `verify_command` (optional string, ≤ 8 KiB)
  added to the `apply_code_patch` tool spec in
  [`internal/middleware/tools.go`](./internal/middleware/tools.go).
  `Validate(ToolApplyCodePatch, …)` type-checks and length-caps it.
- [x] **Snapshot-aware fsops.** New
  `Engine.ApplyCodePatchWithSnapshot` and `Engine.RestoreFile` in
  [`internal/fsops/run.go`](./internal/fsops/run.go) return the pre-edit
  file bytes alongside the apply result and provide a scope-checked
  restore primitive. `applyCodePatchPlan` carries the original bytes so
  the snapshot is captured during the same read that drives the
  TOCTOU-closing dry-run — no extra disk hit.
- [x] **Composition path.** `CompositeDispatcher.applyCodePatchWithVerify`
  in [`internal/middleware/dispatcher.go`](./internal/middleware/dispatcher.go)
  applies the patch, dispatches `verify_command` as an `execute_script`
  run in the same workspace, streams its chunks through the same
  channel the caller already consumes, and on any non-zero exit /
  runner failure restores the file and appends a `rolled back` stderr
  notification. The terminal frame carries the verify command's exit
  code with no `SandboxErr*` code, so `ShouldAutoRetry` treats it as
  retryable and the recovery loop feeds the verify stderr back to the
  translator.
- [x] **Approval surfacing.** `Server.buildApprovalPreview` in
  [`internal/wsserver/sandbox.go`](./internal/wsserver/sandbox.go)
  copies `verify_command` into the approval `preview` payload alongside
  the diff so the operator sees what will run AND that a non-zero exit
  will roll the patch back. The mobile ApprovalSheet renders a new
  "Verify after apply" row labeled "rollback on non-zero exit"
  ([`mobile/src/components/ApprovalSheet.tsx`](./mobile/src/components/ApprovalSheet.tsx)).
- [x] **Test coverage.** Unit tests in
  [`internal/middleware/tools_test.go`](./internal/middleware/tools_test.go)
  pin schema validation; round-trip and out-of-root tests in
  [`internal/fsops/engine_test.go`](./internal/fsops/engine_test.go)
  exercise `ApplyCodePatchWithSnapshot` and `RestoreFile`; end-to-end
  composition tests in
  [`internal/middleware/dispatcher_apply_verify_test.go`](./internal/middleware/dispatcher_apply_verify_test.go)
  cover verify-success, verify-failure-rollback,
  dispatch-error-rollback, missing-sandbox fast-fail, and the
  empty-string fallback to the plain fsops path. The mobile
  ApprovalSheet test asserts the verify row renders only when the
  preview carries one.

See
[`docs/middleware.md`](./docs/middleware.md#verify_command--apply-verify-rollback-on-failure-phase-14)
for the dispatcher composition walkthrough.

### Phase 15: Concurrent worker pool — done
*Closes the "one big migration runs strictly serially" gap: a refactor
that touches a dozen independent files used to be one long tool-call
chain, each edit waiting on the last. `dispatch_worker_pool` lets the
orchestrator fan a migration out across isolated git worktrees, fork the
conversation context into one headless sub-dispatcher per sub-task, run
them in parallel under a concurrency cap, and merge each finished branch
back into the primary branch. Opt-in and off by default — it grants the
orchestrator a new host-side `git` privilege.*

- [x] **New `dispatch_worker_pool` tool.** Tool spec, arg/result types,
  `ParseWorkerPoolArgs`, and the up-front disjointness validator live in
  [`internal/middleware/workerpool.go`](./internal/middleware/workerpool.go);
  the tool is registered in `IsMutatingBaseTool` / `KnownTool` / `Validate`
  in [`internal/middleware/tools.go`](./internal/middleware/tools.go) and
  appended to the catalogue (with approver gating) in
  [`internal/middleware/factory.go`](./internal/middleware/factory.go)
  only when `NOMADDEV_WORKER_POOL_ENABLED=true`.
- [x] **Host-side git control plane.** New
  [`internal/gitctl`](./internal/gitctl/git.go) package shells out to the
  host `git` binary (worktree add/remove, commit, merge) against
  `NOMADDEV_SANDBOX_WORKSPACE_DIR`, which must be a pre-cloned git repo.
  Every invocation passes `-c core.hooksPath=/dev/null` (repo-supplied
  hooks never run — they would be host RCE), plus `GIT_CONFIG_NOSYSTEM=1`,
  `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`, and a fixed argv
  (no shell).
- [x] **Headless sub-dispatchers.** `runWorkerPool` /
  `dispatchOneTask` / `runWorkerToolCall` in
  [`internal/wsserver/workerpool.go`](./internal/wsserver/workerpool.go)
  create one git worktree + temp branch per sub-task under
  `<workspace>/.nomaddev-worktrees/<id>`, fork the parent session's
  windowed conversation history into an independent headless turn loop
  seeded with that sub-task's prompt, and run the loop confined to the
  worktree.
- [x] **Approval gate intact.** The `dispatch_worker_pool` launch is a
  mutating tool and takes one human approval; every mutating tool call a
  headless sub-dispatcher makes (`write_patch`, `apply_code_patch`,
  `execute_script`, …) still goes through the normal human-approval
  round-trip. Nothing is auto-granted.
- [x] **Conflict-free by construction.** Each sub-task declares a `paths`
  array of files/dirs it will modify; the orchestrator rejects the call
  up front if any two scopes overlap (equal or nested). After a
  sub-dispatcher finishes, a post-commit `git diff` check verifies the
  changed files stayed inside the declared scope — a task that escaped is
  marked `scope_violation` and is **not** merged (its branch is kept for
  inspection). Disjoint scopes mean the merge-back never conflicts.
- [x] **Fork-bomb guard.** A sub-dispatcher's tool catalogue
  (`SubDispatcherTools`) excludes `dispatch_worker_pool` — workers cannot
  spawn pools.
- [x] **Bounds.** `NOMADDEV_WORKER_POOL_MAX` (default `4`) is a
  server-wide concurrency semaphore; `NOMADDEV_WORKER_POOL_MAX_TASKS`
  (default `8`) caps the `tasks` array length;
  `NOMADDEV_WORKER_POOL_TASK_TIMEOUT` (default `10m`) is a
  per-sub-dispatcher wall-clock timeout. Worktrees are removed after the
  pool; temp branches are deleted for merged tasks and kept for failed /
  scope-violated ones. Requires `NOMADDEV_SANDBOX_PER_SESSION_WORKSPACE=false`
  — the tool returns a clear error otherwise.
- [x] **Wire + metrics.** New `worker.update` event
  ([`internal/event/types.go`](./internal/event/types.go)) streams
  sub-task lifecycle progress; three Prometheus instruments
  (`nomaddev_worker_pool_dispatches_total`, `nomaddev_worker_pool_tasks_total`,
  `nomaddev_worker_pool_task_seconds`) land in
  [`internal/metrics/metrics.go`](./internal/metrics/metrics.go).

See [`docs/middleware.md`](./docs/middleware.md#dispatch_worker_pool--concurrent-worktree-migration-phase-15)
for the orchestration walkthrough,
[`docs/events.md`](./docs/events.md#worker-pool-progress) for the
`worker.update` wire shape, and
[`docs/adr/0002-concurrent-worker-pool.md`](./docs/adr/0002-concurrent-worker-pool.md)
for the design decisions.

### Phase 16: Native Go mobile apps — in progress
*Objective: ship a real native phone app (Android first, iOS later) as a
second interface alongside the React Native SPA. The orchestrator and wire
protocol are unchanged; the new client speaks the same v1 envelopes.*

Built on [Gio](https://gioui.org) — a pure-Go cross-platform UI toolkit that
produces real APKs and IPAs through `gogio`, the same toolchain Tailscale's
mobile apps use. The native app lives at
[`cmd/nomaddev-mobile/`](./cmd/nomaddev-mobile/), and the supporting
packages at [`internal/mobile/`](./internal/mobile/) and
[`internal/wireclient/`](./internal/wireclient/).

#### 16.1 Foundation (M1) — done
- [x] **New `internal/wireclient` package** extracted from
  [`cmd/wsclient/main.go`](./cmd/wsclient/main.go) so the CLI smoke tool
  and the native mobile binary share one transport. Owns dialing (Authorization
  header or `Sec-WebSocket-Protocol: bearer, <jwt>` subprotocol), per-frame
  timeouts, and envelope read/write. Existing wsclient stdout / flags / exit
  codes are unchanged so [`infra/scripts/smoke.sh`](./infra/scripts/smoke.sh)
  continues to work.
- [x] **Gio scaffolding at [`cmd/nomaddev-mobile/`](./cmd/nomaddev-mobile/)**
  — a placeholder window that confirms the build path end-to-end. The package
  is build-constrained to platforms Gio supports without extra C deps; a
  no-op stub keeps `go list ./...` happy on Linux CI runners.
- [x] **Makefile targets** `android-tools`, `android-debug` (cross-compiles
  to `build/android/nomaddev.apk` via `gogio -target android`),
  `android-install`, `android-clean`.
- [x] **CI job `mobile-native-android`** in
  [`.github/workflows/ci.yml`](./.github/workflows/ci.yml): JDK 17 + Android
  SDK 34 + NDK 25, builds an unsigned debug APK on every PR and uploads it
  as a 14-day artifact.

#### 16.2 Onboard + minimal Chat (M2) — done
- [x] **Long-lived `wireclient.Session`** in
  [`internal/wireclient/session.go`](./internal/wireclient/session.go).
  Auto-reconnects with exponential backoff (1s → 30s cap), drains a 64-deep
  outbox of queued `user.intent` envelopes on reconnect, replays missed
  events via `client.hello{last_event_id}`, and surfaces status transitions
  (`idle|connecting|open|closed|unauthorized`) plus inbound envelopes via
  callbacks. Stops the loop on 401/403 instead of reconnect-spinning.
- [x] **State store** at
  [`internal/mobile/state/state.go`](./internal/mobile/state/state.go) mirrors
  the Zustand slices in `mobile/src/state/store.ts`. `Subscribe` returns a
  coalescing channel + unsubscribe; `Update` runs a callback against an
  addressable state copy under one mutex and fans out to subscribers.
  `Ingest` in `ingest.go` routes inbound envelopes (`hello`,
  `assistant.chunk`, `assistant.message`, `error`, `ack`) into the store.
- [x] **`TokenStore` interface** in
  [`internal/mobile/state/tokens.go`](./internal/mobile/state/tokens.go).
  M2 ships a plain JSON file under `os.UserConfigDir()/nomaddev/`; M6 will
  swap in Android-Keystore-backed AES-GCM behind the same interface.
- [x] **Gio screens** at [`internal/mobile/ui/`](./internal/mobile/ui/):
  `Onboard` (server URL + JWT entry with masked token, error surface),
  `Chat` (turn list with user/assistant bubbles, connection status header,
  text-only composer), and an `App` shell that subscribes to the store,
  drives the session in a background goroutine, and switches screens
  based on auth state. Android's Back button signs the user out.
- [x] **Test coverage.** `internal/wireclient/session_test.go` covers
  status transitions, outbox cap (drops oldest), outbox drain on reconnect,
  401 → unauthorized stop, and backoff saturation, all with a fake
  WebSocket server. `internal/mobile/state/state_test.go` covers store
  initial state, sign-out clearing, subscribe-coalesce, the full ingest
  reducer, file-backed token roundtrip, and concurrent-update race safety.

See [`docs/mobile-native.md`](./docs/mobile-native.md) for the architecture,
build instructions, and onboarding flow.

#### 16.3 ApprovalSheet + LiveTerminal (M3) — done
- [x] **State extensions.** `state.Turn` now carries `ToolCalls []ToolCall`;
  `state.State` gains `PendingApprovals []ApprovalRequest`. New
  `MergeChunkIntoToolCall` is a pure function exported for direct unit
  tests of the partial-line buffering and roll-off invariants. `Store.PopApproval(envelopeID)`
  removes a pending approval atomically and returns it so the caller can
  compose the correlated reply.
- [x] **Reducer extensions** in
  [`internal/mobile/state/ingest.go`](./internal/mobile/state/ingest.go).
  `command.request` attaches a `ToolCall` to the matching `Turn`;
  `command.chunk` folds incoming bytes through the partial-line buffer,
  rolling off lines beyond `TerminalLineCap` (2000) and force-flushing a
  runaway partial past `PartialLineCap` (64 KiB); `command.result` closes
  the call; `sandbox.heartbeat` updates `ElapsedMs`;
  `tool.approval.request` pushes an `ApprovalRequest` and marks the
  associated `ToolCall.AwaitingApproval` so the surface can render the
  pending state without consulting the approvals slice.
- [x] **`ApprovalSheet`** at
  [`internal/mobile/ui/approval.go`](./internal/mobile/ui/approval.go) —
  modal overlay rendering the tool name (with a GitHub badge for
  `github_*` tools), reason, raw args, an optional unified-diff preview
  with the SPA's per-line colour scheme (green for `+`, red for `-`,
  muted for hunk headers, foreground for context), the optional Phase-14
  `verify_command` row, a one-second-precision countdown derived from
  the request deadline, a type-to-confirm field that gates the Approve
  button on the tool name, and a separate optional deny-reason field.
- [x] **`LiveTerminal`** at
  [`internal/mobile/ui/terminal.go`](./internal/mobile/ui/terminal.go) —
  scrollable list of `TerminalLine` entries with stderr lines coloured
  red, an auto-tail policy that pins to the latest line until the
  operator scrolls up, a "Jump to bottom" pill that re-arms tail when
  tapped, a header showing `live`/`done` status + extrapolated elapsed
  time (`MM:SS` / `H:MM:SS` formatting matching the SPA), and a
  "showing N of M" indicator when the line cap has dropped older rows.
- [x] **Chat surface integration** in
  [`internal/mobile/ui/chat.go`](./internal/mobile/ui/chat.go). Each
  turn's `ToolCall`s render inline under the assistant bubble — one
  `LiveTerminal` widget instance per `CommandID`, cached across frames
  so scroll position survives re-renders.
- [x] **App shell glue** in
  [`internal/mobile/ui/app.go`](./internal/mobile/ui/app.go). When
  `PendingApprovals` is non-empty the `ApprovalSheet` paints over the
  chat surface; Approve sends a `tool.approval.granted` correlated on
  the request envelope ID, Deny sends `tool.approval.denied` with the
  optional reason. A 1 Hz ticker calls `Window.Invalidate` so the
  countdown stays smooth even when no other state changes.
- [x] **Test coverage.** New tests in
  [`internal/mobile/state/state_test.go`](./internal/mobile/state/state_test.go)
  cover command-request → tool-call attachment, mid-line chunk
  buffering across stdout/stderr interleaving, the line-cap drop-oldest
  invariant exercised through the pure `MergeChunkIntoToolCall`, the
  result and heartbeat paths, the full approval push +
  `PopApproval` round-trip, and the `apply_code_patch` preview shape.

#### 16.4 Image attachments (M4) — done
- [x] **State additions** in
  [`internal/mobile/state/state.go`](./internal/mobile/state/state.go).
  `State.PendingImages` holds wire-ready `event.ImageInput` entries the
  composer has queued. `Store.AddPendingImage(img, decodedBytes)`,
  `RemovePendingImage(idx)`, and `TakePendingImages()` enforce
  `MaxImageCount` (4) and `MaxImageBytes` (5 MiB) — the same caps the
  orchestrator's `user.intent` validator applies — and return typed
  `ImageAttachmentError` sentinels so the composer can match errors
  rather than parse strings. Sign-out clears the queue.
- [x] **`DecodeImageAttachment`** in
  [`internal/mobile/state/images.go`](./internal/mobile/state/images.go)
  reads up to `MaxImageBytes+1` bytes from a picked `io.Reader`,
  detects the MIME type via `http.DetectContentType`, falls back to a
  filename-extension lookup for SAF content URIs that strip the
  extension, and base64-encodes the bytes into an `event.ImageInput`.
  Whitelist matches the orchestrator's accepted set
  (`jpeg`/`png`/`gif`/`webp`).
- [x] **Cross-platform image picker** wired through
  [`gioui.org/x/explorer`](https://pkg.go.dev/gioui.org/x/explorer).
  `App` owns one `*explorer.Explorer` per window and feeds every Gio
  event through `Explorer.ListenEvents` so Android's
  `ACTION_GET_CONTENT` callback / iOS's `UIDocumentPickerViewController`
  / desktop's native file dialog all surface back into the app. The
  Android path uses explorer's bundled `file_android.jar` which gogio
  embeds automatically — no extra build step.
- [x] **Composer integration** in
  [`internal/mobile/ui/chat.go`](./internal/mobile/ui/chat.go). New
  `+image` button next to the Send button; a horizontal strip above
  the composer renders one chip per queued attachment (MIME tail label
  + tap-to-remove × badge). Send accepts an attachment-only submit
  (text *or* at least one image) matching the SPA composer.
- [x] **`App.sendIntent` updated**: drains `PendingImages` atomically
  via `TakePendingImages` and ships them on the outbound
  `UserIntentPayload.Images`. The user-bubble preview path remains the
  same — `RecordSentIntent` already accepts the images slice.
- [x] **Test coverage.** Five new state tests cover the count cap, the
  per-attachment byte cap, the MIME-type whitelist, the Remove +
  out-of-range no-op + Take semantics, PNG signature detection through
  `DecodeImageAttachment`, the extension fallback for SAF content
  URIs, the non-image rejection path, and the sign-out clear.
  `go test -race ./internal/...` green; `golangci-lint run ./...`
  0 issues; desktop build with the new explorer dependency succeeds.

#### 16.5 Settings + read-only Config viewer (M5) — done
- [x] **Screen navigation.** New `state.Screen` enum (`chat | settings |
  config`) plus `Store.SetScreen`. The App shell's frame switch routes
  to the matching widget; the chat header's new ⚙ button flips to
  Settings; Back / sign-out / reset paths route between them cleanly.
- [x] **Settings screen** at
  [`internal/mobile/ui/settings.go`](./internal/mobile/ui/settings.go) —
  connection metadata block (server URL, status, session ID, last event
  ID, outbox depth), the cumulative session-token + cost ticker, the
  model picker (when the orchestrator's `hello` advertised
  `available_models`; selected row highlighted, tap sends
  `user.command{set_model}`), the last-error block, and four actions:
  Reset history (sends `user.command{reset_history}` and clears local
  `Turns` / `SessionTokens` immediately via the new `Store.ResetTurns`),
  Force reconnect (rebuilds the `wireclient.Session` against saved
  creds), Open server config (navigates + kicks a fetch), Sign out
  (clears the token, returns to Onboard).
- [x] **`AdminClient`** at
  [`internal/mobile/state/admin.go`](./internal/mobile/state/admin.go).
  Typed `ConfigSetting` / `ConfigSnapshot` mirroring
  `internal/wsserver/config_handlers.go`. `DeriveHTTPBase` normalises
  the WebSocket URL the App already has on file (`ws://host/ws`) into
  the HTTP base the admin endpoints anchor at (`http://host`), with
  HTTPS / explicit-http / trailing-path / missing-host cases covered.
  `FetchConfig(ctx)` hits `GET /admin/config` with the bearer token
  and surfaces the server's body verbatim on 401 / 403 so the
  operator knows whether to re-onboard or ask for `config:read`.
- [x] **Read-only Config viewer** at
  [`internal/mobile/ui/config.go`](./internal/mobile/ui/config.go).
  Settings grouped by category with one collapsible header per group
  (▸ / ▾, drilldown survives re-fetch), per-row display of env var,
  type, dangerous / read-only flags, value (`(secret set)` /
  `(unset)` for secret rows), and help text. Refresh button kicks a
  fresh `GET /admin/config`; loading and error states render inline.
- [x] **Test coverage.** Four new tests in
  [`internal/mobile/state/admin_test.go`](./internal/mobile/state/admin_test.go)
  cover `DeriveHTTPBase` across the URL shapes the app actually sees
  (ws/wss/http/https/trailing path/missing host/unsupported scheme),
  the happy-path GET round-trip with bearer auth, the 403 error
  surface, and the WS-URL-on-the-Onboard-screen → HTTP-call path.

**Deferred to M6 (Android hardening + first ship):** the schema-driven
Config **editor** (dirty tracking, type-driven field widgets,
type-to-confirm for dangerous fields, Apply + Restart with 35 s
timeout and 2.5 s polling reconnect) and Android-Keystore-backed
AES-GCM token storage. The read-only viewer this milestone ships proves
the endpoint contract end-to-end so the editor work in M6 starts from
a known-good base.

#### 16.6 Schema-driven Config editor + restart-reconnect loop (M6.1) — done
- [x] **`AdminClient` extended** with `ApplyConfig(ctx, changes, reset)`
  and `RestartOrchestrator(ctx)`. PUT carries `{changes, reset}`; per-field
  rejections surface as a typed `*state.ApplyConfigError` with a populated
  `EnvVar` field so the editor can highlight the offending row.
  Cross-field invariants and missing-scope failures come back with
  `EnvVar == ""` and route to the banner instead.
- [x] **State plumbing.** `State.RestartPending` flips to `true` after
  `POST /admin/config/restart` and resets when the next `hello` arrives,
  giving the polling loop a single source of truth. `Store.SetRestartPending`
  is the explicit setter; the reducer in
  [`internal/mobile/state/ingest.go`](./internal/mobile/state/ingest.go)
  flips it to `false` on every `hello` so a reconnect after the restart
  drives the editor into `ConfigPhaseApplied`.
- [x] **Editor widget** at
  [`internal/mobile/ui/config.go`](./internal/mobile/ui/config.go).
  Dirty tracking per row + a category dirty-count badge. Type-driven
  fields: bool / enum render as tap-to-cycle buttons; string / int /
  duration / csv / float / int64 all share a text editor and rely on
  the server's per-field validator. Read-only rows render the current
  value as muted text. Secrets display `(secret set)` / `(unset)` when
  empty. Each dirty row sprouts a Revert button; the footer carries a
  Revert-all plus the Apply button. Dangerous changes gate Apply behind
  an explicit "Acknowledge dangerous changes" confirmation and the
  Apply button itself turns red.
- [x] **Apply + Restart + polling state machine** in
  [`internal/mobile/ui/app.go`](./internal/mobile/ui/app.go). Five phases
  (`idle | applying | restarting | applied | reauth | failed`) match the
  SPA's vocabulary. PUT runs on a goroutine; on 401 the editor flips to
  `reauth` with a sign-out button; on field error it highlights the row
  and opens the category. On PUT success the WS session tears down,
  `POST /admin/config/restart` fires, and `driveRestartPolling`
  rebuilds the session with 2.5 s checks against a 35 s budget. Success
  re-fetches `GET /admin/config` so the operator sees the new effective
  values.
- [x] **Test coverage.** Four new tests in
  [`internal/mobile/state/admin_test.go`](./internal/mobile/state/admin_test.go)
  cover the PUT happy path (asserts the wire body shape against an
  `httptest` server), the per-field rejection path (verifies the
  `*ApplyConfigError` typed error carries `Status`, `EnvVar`, and
  `Message`), the non-JSON 403 (config:write scope missing) surface,
  and the restart endpoint tolerating both `200 OK` and `202 Accepted`.

**Still deferred to M6.2 (Android Keystore + signed APK ship):**
Android-Keystore-backed AES-GCM token storage via JNI, deep-link intent
filter, release-keystore signing, and the on-device smoke gate.

#### 16.7 Signed release APK + deep-link intent filter (M6.2) — done
- [x] **`-schemes nomaddev` registered** in both
  [`make android-debug`](./Makefile) and the new `make android-release`,
  so gogio emits the `<intent-filter>` Android needs to route
  `nomaddev://` URLs to the app.
- [x] **Deep-link handler** in
  [`internal/mobile/ui/app.go`](./internal/mobile/ui/app.go).
  `App.HandleURL(*url.URL)` accepts the native shape
  (`nomaddev://onboard?server=…&token=…&sid=…`) **and** the SPA's
  fragment shape (`https://orch/#token=…&sid=…`) so a single QR works
  on both clients. The pure parser lives at
  [`internal/mobile/state/deeplink.go`](./internal/mobile/state/deeplink.go)
  and is unit-tested independently of Gio.
- [x] **`app.Events` plumbing** in
  [`cmd/nomaddev-mobile/main.go`](./cmd/nomaddev-mobile/main.go).
  Replaces `app.Main()` with the iterator-style entry point so
  `app.URLEvent` reaches the app whether or not a window is currently
  in the foreground.
- [x] **Signed-APK Makefile path** at
  [`Makefile`](./Makefile). `android-release` reads `ANDROID_KEYSTORE`,
  `ANDROID_KEYSTORE_PASS`, and `ANDROID_VERSION` from the environment
  and hands them to gogio's `-signkey` / `-signpass`. A
  `android-debug-keystore` helper generates a throwaway PKCS12
  keystore for local smoke testing — strictly never used for real
  releases (its password is published in the Makefile).
- [x] **CI release pipeline** updated at
  [`.github/workflows/release.yml`](./.github/workflows/release.yml).
  New `android-apk` job provisions JDK 17 + Android SDK 34 + NDK 25,
  installs gogio, decodes the keystore from
  `secrets.ANDROID_KEYSTORE_BASE64`, and signs the APK with
  `secrets.ANDROID_KEYSTORE_PASS`. Missing secrets degrade
  gracefully to an unsigned APK so the release still produces an
  artifact. The `release` job now attaches `nomaddev.apk` to the
  GitHub Release alongside the orchestrator binaries.
- [x] **`docs/mobile-android.md`** documents the keystore generation,
  the env-var contract, the release pipeline, and the
  `nomaddev://` + SPA-fragment URL shapes the app accepts.

**Still deferred to M6.3 (on-device first-ship gate):**
Android-Keystore-backed AES-GCM token storage via JNI (the M5 file-
backed store is the floor; an attacker with debug-bridge access to an
unlocked device can read it). On-device smoke tests against a real
orchestrator over Tailscale, including the deep-link round trip.

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

For the Phase 8 access/refresh flow, mint both at once and use the
refresh endpoint to rotate the access token without re-running
`gen-jwt`:

```sh
PAIR="$(go run ./scripts/gen-jwt -kind pair -sub matt -sid sess-1)"
ACCESS="$(echo "$PAIR" | jq -r .access_token)"
REFRESH="$(echo "$PAIR" | jq -r .refresh_token)"

# Use ACCESS at /ws (above). Later, exchange REFRESH for a new pair:
curl -sS -X POST http://127.0.0.1:8080/auth/refresh \
    -H "Authorization: Bearer $REFRESH" | jq .

# Revoke a token before it expires naturally:
curl -sS -X POST http://127.0.0.1:8080/auth/revoke \
    -H "Authorization: Bearer $ACCESS" -o /dev/null -w '%{http_code}\n'
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
`requireKey(t)` and skip when `NOMADDEV_GEMINI_API_KEY` is absent. The
OpenAI- and Anthropic-tagged tests
(`internal/middleware/{openai,anthropic}_test.go`) drive the translators
against an `httptest` SSE stub, so they run in CI without any API key.

Build the Docker-enabled binaries with `make build-docker`, or pick an
LLM backend with `make build-gemini`, `make build-openai` (which also
enables `runtime=deepseek`), or `make build-anthropic`. `make build-all`
links Docker, GitHub MCP, and all three LLM SDKs into one binary. See
[`.env.example`](./.env.example) for all configuration knobs.

---

## 🚢 Deploying

**Prerequisites:** A fresh Ubuntu VPS (any provider — verified on Hetzner
CX22 / CAX11), a Tailscale account. No DNS, no certificate, no extra
infrastructure.

### Easiest — one command

On a fresh Ubuntu VPS, install everything in one shot:

```sh
curl -fsSL https://raw.githubusercontent.com/MattCheramie/NomadDev/main/install.sh | sudo bash
```

[`install.sh`](./install.sh) installs the prerequisites (base packages,
Tailscale, and Docker when the Docker path is used), clones the repo to
`/opt/nomaddev`, and runs the matching quickstart below. It auto-detects the
deploy path; force one with `--docker` / `--systemd` (when piped, pass flags
after `-s --`: `… | sudo bash -s -- --docker`). Idempotent — re-run to update.

### Or pick a path manually

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
*   **Human-in-the-Loop:** Destructive commands parsed by the middleware require explicit operator approval on the mobile client. The default UX requires the operator to **type the exact tool name** before the Approve button enables — typed confirmation works over plain HTTP via Tailscale (the default deploy). Operators who front the orchestrator with a TLS reverse proxy can layer WebAuthn / platform-biometric authenticators on top; see Phase 8.6 below.

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
