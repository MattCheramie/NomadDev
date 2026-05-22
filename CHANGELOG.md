# Changelog

All notable changes to NomadDev are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-05-22

Initial public release. NomadDev is an experimental, mobile-first remote
execution environment: a natural-language interface for managing a headless
VPS, running code in disposable sandboxes, and driving GitHub — all over a
private Tailscale mesh, with no public SSH port.

### Added

#### Orchestrator

- Concurrent WebSocket server (`cmd/orchestrator`) with JWT-authenticated
  connections, a JSON event protocol, and state recovery for dropped clients
  via a per-session replay ring buffer.
- Persisted, editable orchestrator settings exposed over `/admin/config`.
- `/healthz` and `/readyz` dependency probes for load balancers and container
  healthchecks.
- Prometheus metrics, OpenTelemetry tracing with `traceparent` propagation
  from the mobile client, and a structured security-audit event sink.

#### Ephemeral sandbox

- Docker-backed one-shot containers per tool call: no network, read-only
  rootfs, `runsc` (gVisor) isolation when the host advertises it, and hard
  memory / CPU / pids / wall-clock limits.
- Per-session workspace isolation and container image digest pinning with
  verification.
- Tools: `execute_script`, `read_file`, `write_patch`, `apply_code_patch`
  (dry-run diff preview with verify-command rollback), `search_syntax`
  (ast-grep), `lsp_query` (semantic code navigation), `monitor_daemon`, and a
  Live Terminal for long-running jobs.

#### NLP-to-RPC middleware

- Pluggable LLM backends — Google Gemini, OpenAI, Anthropic, and DeepSeek —
  each behind its own build tag and selectable via
  `NOMADDEV_MIDDLEWARE_RUNTIME`.
- Natural-language intent to JSON tool-call dispatch loop, with an
  audit / dry-run mode that strips mutating tools from the catalogue.
- `fetch_external_docs` tool with an SSRF guard and outbound exfiltration
  screening (credential, secret-pattern, and high-entropy token detection;
  optional documentation-domain allowlist).
- `dispatch_worker_pool` for concurrent codebase migrations,
  `pin_file` / `unpin_file` persistent reference buffer, background
  summarization compaction for long sessions, bounded automated error
  recovery, multimodal image inputs, Anthropic extended-thinking frames, a
  per-request retry budget, and USD cost accounting.

#### GitHub MCP backend

- Subprocess-managed embedding of the official `github-mcp-server`, exposing
  ~75 GitHub operations as tool calls with per-user PATs, argument caps,
  sensitive-argument redaction, rate-limit-aware bounded retry, and
  upstream-drift CI.

#### Approval and security

- Mutating operations gated behind a typed-confirmation approval flow.
- Per-tool JWT scopes, refresh tokens with a JTI revocation list, and
  signing-key rotation.
- WebAuthn / security-key registration and authentication ceremonies, on both
  the server and the mobile SPA.
- Origin allowlist, Content-Security-Policy, per-connection rate limiting, and
  a WebSocket body-size cap.

#### Mobile Control Hub

- React Native (Expo) client: onboarding, chat with streaming tool output, an
  approval sheet, runtime model switching, a server-configuration editor, and
  settings.

#### Persistence

- SQLite-backed session and history stores with an integrity check,
  forward-only schema migrations, and an automated daily backup timer.

#### Deployment and supply chain

- One-command VPS install (`install.sh`) plus Docker and systemd quickstarts,
  a multi-stage distroless Dockerfile, `docker-compose`, a cloud-init
  template, a Tailscale ACL example, and systemd units.
- Tag-driven release workflow producing Linux amd64/arm64 binaries and a
  multi-arch GHCR image, each shipped with an SPDX SBOM and a keyless cosign
  signature.
- CI across all provider build tags, race tests, a coverage floor,
  `golangci-lint`, Trivy, and `govulncheck`.

#### Documentation

- Operator guide as the single deploy entry point, plus architecture, events,
  auth, sandbox, middleware, GitHub, WebAuthn, approval, operations, privacy,
  and supply-chain references, and architecture decision records.

[0.1.0]: https://github.com/mattcheramie/nomaddev/releases/tag/v0.1.0
