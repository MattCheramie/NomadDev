# NomadDev Operations

Reference for running, monitoring, and releasing the orchestrator. The
companion document is [`infra/RUNBOOK.md`](../infra/RUNBOOK.md), which
covers first-deploy and incident response. This file focuses on the
post-deploy operational surface.

## Metrics

The orchestrator exposes Prometheus instruments at `/metrics`. The
endpoint is unauthenticated by design — restrict access at the network
layer (Tailscale ACLs or ufw on `tailscale0`). All instruments are
defined in [`internal/metrics/metrics.go`](../internal/metrics/metrics.go).

| Name                                  | Type      | Labels    | Notes                                            |
| ------------------------------------- | --------- | --------- | ------------------------------------------------ |
| `nomaddev_ws_connects_total`          | counter   | `result`  | `ok` / `unauthorized` / `upgrade_failed`         |
| `nomaddev_ws_active_connections`      | gauge     | —         | Live WebSocket clients                           |
| `nomaddev_session_events_total`       | counter   | `kind`    | One label per envelope `type` flowing into replay |
| `nomaddev_sandbox_runs_total`         | counter   | `outcome` | `ok` / `timeout` / `canceled` / `oom` / etc.     |
| `nomaddev_sandbox_run_seconds`        | histogram | —         | 10 ms → ~40 s buckets                            |
| `nomaddev_middleware_turns_total`     | counter   | `outcome` | `ok` / `error`                                   |
| `nomaddev_middleware_turn_seconds`    | histogram | —         | 50 ms → ~3 min buckets                           |

Suggested alerts:
- `rate(nomaddev_ws_connects_total{result="unauthorized"}[5m]) > 1` —
  someone is probing with bad JWTs.
- `histogram_quantile(0.95, rate(nomaddev_sandbox_run_seconds_bucket[5m]))
  > 30` — p95 sandbox run is approaching the default timeout.
- `nomaddev_ws_active_connections > 0` for `< 5m` while
  `rate(nomaddev_middleware_turns_total[5m]) == 0` — the daemon is up
  but no turns are completing.

## Persistent state

All persistent files live under `/var/lib/nomaddev` (override via
`NOMADDEV_SESSION_PATH`, `NOMADDEV_HISTORY_PATH`, and
`NOMADDEV_SANDBOX_WORKSPACE_DIR`):

| File                | Owner       | Contents                                            |
| ------------------- | ----------- | --------------------------------------------------- |
| `sessions.db`       | session.SQLiteStore | Replay-buffer rows + per-SID `last_seen`     |
| `history.db`        | history.SQLiteStore | Conversation turns + tool calls/results       |
| `work/`             | fsops + sandbox     | Per-session workspace bind-mounted into runner |

Both `.db` files are SQLite WAL. Back them up while the orchestrator is
stopped, or use `sqlite3 sessions.db ".backup target.db"` while live.

If `sessions.db` is unwritable at start (e.g. permission drift), the
orchestrator logs a warning and falls back to the in-memory store. The
`/healthz` endpoint still returns 200; check `journalctl` (or
`docker compose logs`) for `session: sqlite open failed, falling back to
memory` to detect this.

## Release process

Releases are tag-driven:

```sh
git tag -a v0.4.0 -m "v0.4.0"
git push origin v0.4.0
```

The `.github/workflows/release.yml` workflow runs three jobs:

1. **binaries** (matrix amd64 + arm64) — produces statically-linked
   `orchestrator-linux-${arch}` binaries with `.sha256` files.
   `main.version` is stamped from the tag via `-ldflags`.
2. **github release** — creates a release with auto-generated notes and
   attaches the four artifacts.
3. **ghcr image** — buildx-builds the multi-arch image and pushes
   `ghcr.io/${owner}/${repo}:${tag}` plus `:latest`.

Verify a release with `./bin/orchestrator -version` (prints the injected
tag) and `docker pull ghcr.io/${owner}/${repo}:${tag}`.

The CI `docker-image` job builds the Dockerfile on every PR (no push),
so Dockerfile breakage is caught before a tag is cut. If it goes red,
do not tag.

## Common operational tasks

### Rotating the JWT secret
See [`infra/RUNBOOK.md`](../infra/RUNBOOK.md). Existing clients lose
their tokens; re-onboard via QR.

### Resetting one user's history
The mobile Settings screen exposes a "Reset history" button that emits
a `user.command{reset_history}` envelope. The server calls
`history.Store.Reset(sid)` and acks. From a shell:

```sh
sqlite3 /var/lib/nomaddev/history.db "DELETE FROM turns WHERE sid = '<sid>';"
```

### Resizing replay buffers
`NOMADDEV_SESSION_BUFFER_SIZE` (count) and `NOMADDEV_SESSION_MAX_BYTES`
(bytes) cap the in-memory window. The SQLite write-through trim keeps at
most ~2× the count per SID, so increasing the buffer also grows the
on-disk footprint.

### Capacity throttling
- Sandbox runs: `NOMADDEV_SANDBOX_MAX_CONCURRENT`. Above this, new
  `command.request` envelopes get `sandbox_unavailable` immediately.
- Middleware turns: `NOMADDEV_MIDDLEWARE_MAX_CONCURRENT`. Above this,
  new `user.intent` envelopes get a synthetic error `assistant.message`.

### Troubleshooting checklist
1. `curl /healthz` returns 200.
2. `curl /metrics | grep nomaddev_ws_connects_total` is increasing.
3. `journalctl -u nomaddev-orchestrator -n 200` (or `docker compose logs
   --tail 200`) — look for warnings (`session: ...`, `middleware: ...`).
4. `bash infra/scripts/smoke.sh` from a tailnet client.
5. Mobile Settings → Force reconnect.

If 1–3 are healthy but the mobile client is stuck, suspect the
client-side wire layer (clear browser storage or re-onboard).
