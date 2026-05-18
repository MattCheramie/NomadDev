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
| `nomaddev_github_calls_total`         | counter   | `tool`, `outcome` | One per `github_*` MCP invocation; outcomes `ok` / `error` / `timeout` / `canceled` / `bad_request` / `denied` |
| `nomaddev_github_call_seconds`        | histogram | —         | 50 ms → ~3 min buckets; only actual upstream round-trips observed |

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

### Integrity check + schema migrations (Phase 8.7)

Every SQLite-backed store (`sessions.db`, `history.db`, the JTI
revocation DB) runs the following on every orchestrator start, in
order:

1. `PRAGMA integrity_check` — refuses to boot if SQLite reports
   anything other than `ok`. Catches page-level corruption that a
   normal query path might miss until the orchestrator is already
   writing.
2. `PRAGMA user_version` is read; any forward-only migrations from
   [`internal/dbutil`](../internal/dbutil/dbutil.go) at versions
   higher than the current value are applied. Each migration runs
   in a transaction that also bumps `user_version`, so a failure
   rolls back atomically and the same migration is retried on the
   next boot.
3. If `user_version` is **higher** than the latest migration this
   binary supports, the constructor returns `ErrSchemaTooNew` and
   the orchestrator refuses to start. Operators see a clear "binary
   downgrade detected" error instead of silent data loss.

To inspect a database's schema version manually:

```sh
sqlite3 /var/lib/nomaddev/sessions.db 'PRAGMA user_version;'
sqlite3 /var/lib/nomaddev/sessions.db 'PRAGMA integrity_check;'
```

Adding a new migration is a single append to the `migrations` slice
in the relevant `internal/{auth,history,session}/*.go` file —
**never edit an existing migration** since older deploys won't
re-run it on upgrade.

### Liveness vs readiness (Phase 8.8)

Two HTTP endpoints answer "is the orchestrator serving":

- **`GET /healthz`** — *liveness*. Returns `200 {"status":"ok"}` as
  long as the HTTP listener can respond. Use this for restart
  decisions (Docker, systemd watchdog) — the process is alive.
- **`GET /readyz`** — *readiness*. Iterates configured dependency
  probes (currently the three SQLite stores: `sessions.db`,
  `history.db`, the revocation DB) with a 2-second per-probe budget,
  and returns either:
  - `200 {"status":"ok","checks":{"session_db":"ok",...}}`
  - `503 {"status":"degraded","checks":{"session_db":"<error msg>",...}}`

  Use this for load-balancer pool membership or alerting — the
  process is alive AND its dependencies are reachable. A failing
  probe is a signal to investigate, not necessarily to restart.

The `docker-compose.yml` shipped with this repo wires its
`HEALTHCHECK` to `orchestrator -healthcheck http://127.0.0.1:8080/readyz`
— Compose flips the container to `unhealthy` after three consecutive
failures and `restart: unless-stopped` bounces it. The
`-healthcheck` flag re-uses the orchestrator binary as its own HTTP
client because the distroless/static base image has no shell or
wget.

For systemd, add `WatchdogSec=30` and a periodic `curl -fsS
http://127.0.0.1:8080/readyz` (or call the binary with
`-healthcheck`) in a small companion timer unit if you want
readiness-based restart semantics on top of liveness. The default
unit relies on the process being alive (Type=simple).

### Automated SQLite backups (Phase 8.10)

The systemd quickstart installs a daily `nomaddev-backup.timer` that
takes online snapshots of every SQLite store (`sessions.db`,
`history.db`, the revocation DB) via `sqlite3 .backup`, verifies each
snapshot with `PRAGMA integrity_check`, gzip-compresses it, and prunes
archives older than the retention threshold.

```sh
# Inspect timer state + last run / next run.
systemctl list-timers nomaddev-backup.timer
systemctl status nomaddev-backup.service

# Run an out-of-cycle backup right now.
sudo systemctl start nomaddev-backup.service

# Override default location / retention via /etc/nomaddev/env or a
# drop-in for nomaddev-backup.service.
# NOMADDEV_DATA_DIR=/var/lib/nomaddev              # source dir
# NOMADDEV_BACKUP_DIR=/mnt/nomaddev/backups        # default ${DATA_DIR}/backups
# NOMADDEV_BACKUP_RETENTION_DAYS=14                # default 14
```

Snapshots land as `sessions.<UTC-timestamp>.db.gz`,
`history.<UTC-timestamp>.db.gz`, etc. The integrity check runs *before*
gzip, so a corrupt source DB fails the timer rather than poisoning the
archive directory.

To restore a snapshot:

```sh
# Stop the orchestrator first — the restore is a file swap.
sudo systemctl stop nomaddev-orchestrator
cd /var/lib/nomaddev
sudo -u nomaddev gzip -dk backups/sessions.20260518T030000Z.db.gz
sudo mv sessions.db sessions.db.bad
sudo -u nomaddev mv backups/sessions.20260518T030000Z.db sessions.db
sudo systemctl start nomaddev-orchestrator
# The orchestrator's startup integrity_check (Phase 8.7) will catch
# any inconsistency in the restored file.
```

Docker / Compose users can run the same script from a host cron job
against the bind-mounted `nomaddev-data` volume:

```cron
17 3 * * *  docker exec nomaddev-orchestrator nomaddev-backup || true
```

(Requires bundling the script + `sqlite3` into the orchestrator
image, OR running the script from a small sidecar — both out of
scope for this phase; the systemd path is the recommended deploy.)

### Single-node only (Phase 11 doc)

NomadDev is **explicitly a single-node deployment** today. Two
orchestrator processes sharing the same Tailscale IP, SQLite
stores, or session-replay state is **not supported** — the
state is kept in-process maps + local SQLite, with no
cross-instance coordination.

What this means in practice:

- **No active-active.** Don't run two orchestrators behind a
  load balancer pointing at the same `/var/lib/nomaddev`. SQLite
  locking will fight you and the in-memory hub state will fork.
- **No active-passive failover.** The session-replay buffer and
  approval-pending state live in RAM; killing the active node
  loses both. A standby would need to rehydrate from `sessions.db`
  and pick up new pending approvals from scratch, which we don't
  ship.
- **High-availability** in this project's vocabulary means
  "operator restarts the systemd unit quickly" — the Phase 8.7
  startup integrity check + Phase 8.8 `/readyz` probe + Phase
  8.10 daily backup are the recovery primitives. Restoring from
  a snapshot takes seconds.

If your deployment requires real HA, the orchestrator's
in-process state is the obstacle. The natural shape would be:

1. Move `sessions.db` / `history.db` / `revocations.db` to a
   shared backend (Postgres, distributed SQLite via Litestream).
2. Make the hub stateless (no in-memory pending-approval map; move
   to the shared backend with a cross-instance pub/sub).
3. Make the audit sink network-attached (Loki, syslog over the
   tailnet).

That's a meaningful refactor; the
[missing-features review](https://github.com/MattCheramie/NomadDev/issues)
captures it as a long-tail item. Single-node + restart-fast is
the supported posture until then.

### Log rotation (Phase 11 doc)

The orchestrator's stdout + stderr go to systemd's journal; the
`audit.log` file (when `NOMADDEV_AUDIT_BACKEND=file`) is the only
plain-text log surface that grows on disk unbounded.

journald handles the systemd-side rotation automatically — see
`man journald.conf` for `SystemMaxUse=`, `SystemKeepFree=`,
`RuntimeMaxUse=`. The defaults (~4 GiB cap) are usually fine on
a real VPS.

The audit log needs `logrotate`. Drop the following at
`/etc/logrotate.d/nomaddev`:

```
/var/lib/nomaddev/audit.log {
    daily
    rotate 30
    compress
    delaycompress
    missingok
    notifempty
    create 0600 nomaddev nomaddev
    # Phase 11.3: SIGHUP tells the orchestrator to close audit.log
    # and reopen at the same path. logrotate ships the rotated
    # file with the existing fd; the post-HUP open lands in a
    # fresh file, so no events get truncated and no in-flight
    # buffer is lost.
    postrotate
        systemctl kill --signal=SIGHUP nomaddev-orchestrator.service > /dev/null 2>&1 || true
    endscript
}
```

`logrotate` runs daily by default via the `/etc/cron.daily/logrotate`
hook on Ubuntu / Debian — no additional cron entry needed. Verify
with `sudo logrotate -d /etc/logrotate.d/nomaddev` (dry run).

The SIGHUP-reopen handler landed in Phase 11.3 — a manual rotation
test is `sudo systemctl kill --signal=SIGHUP nomaddev-orchestrator`
which should produce `audit: reopened on SIGHUP` in the journal
without dropping any in-flight events.

### OpenTelemetry tracing (Phase 11.2)

Tracing is **off** by default — `otel.Tracer(...)` returns a noop
tracer at every call site so the codebase pays only the
tens-of-nanoseconds tracer-noop cost. Flip on per host:

```sh
# /etc/nomaddev/env
NOMADDEV_OTEL_ENABLED=true
NOMADDEV_OTEL_OTLP_ENDPOINT=otel-collector.tailnet:4318
NOMADDEV_OTEL_INSECURE=true            # plain HTTP over Tailscale
NOMADDEV_OTEL_SAMPLE_RATIO=1.0         # tighten in production
NOMADDEV_OTEL_SERVICE_NAME=nomaddev-orchestrator
```

```sh
sudo systemctl restart nomaddev-orchestrator
journalctl -u nomaddev-orchestrator | grep 'tracing: enabled'
```

What's instrumented today:

- **`ws.dispatch.<envelope.type>`** (Phase 11.2) — one root span
  per inbound envelope, with `envelope.type` / `session.sub` /
  `session.sid` attributes.
- **`sandbox.exec`** (Phase 11.3) — per sandbox run, with
  `sandbox.tool` / `sandbox.session_id` / `sandbox.shell` /
  `sandbox.timeout_ms` attributes. Wraps the docker bind-mount
  setup + container lifecycle so the span's wall-clock covers
  the full run.
- **`github.call`** (Phase 11.3) — per `github_*` tool call, with
  `github.tool` / `github.session_id` attributes. Args are
  deliberately omitted from span attributes — they'd dwarf trace
  storage and could leak secrets.

These spans don't yet chain into the `ws.dispatch` root because the
dispatcher's context isn't threaded into `runner.Exec` /
`Client.Call`; that's a follow-up refactor (see README's
remaining-Phase-11 list). For per-stage timing today, pair the
trace surface with the Phase 11.1 Grafana dashboard — Prometheus
already exposes per-stage latency histograms.

**Why a quiet-fallback default.** If the OTLP endpoint is a typo,
`tracing.Init` logs a warning and disables tracing rather than
crashing the orchestrator. The configured collector being down is
a tracing-pipeline problem, not an orchestrator availability
problem.

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

### Rotating the GitHub PAT
The orchestrator's `EnvTokenSource` re-reads `NOMADDEV_GITHUB_TOKEN` on
every tool call, so rotation is: update the env file, restart the
service.

```sh
# Docker compose deploy:
$EDITOR /etc/nomaddev/.env           # set new NOMADDEV_GITHUB_TOKEN=...
docker compose -f /etc/nomaddev/docker-compose.yml restart

# Systemd deploy:
$EDITOR /etc/nomaddev/orchestrator.env
sudo systemctl restart nomaddev-orchestrator
```

Verify with the startup log line `orchestrator: github backend ready
tools=N` (the count is non-zero only when the PAT is valid).

In-flight calls finish under the old credential; the next call uses the
new one. To pre-emptively revoke the old PAT, do so on
github.com/settings/tokens **after** confirming the new one works.

### Troubleshooting checklist
1. `curl /healthz` returns 200.
2. `curl /metrics | grep nomaddev_ws_connects_total` is increasing.
3. `journalctl -u nomaddev-orchestrator -n 200` (or `docker compose logs
   --tail 200`) — look for warnings (`session: ...`, `middleware: ...`).
4. `bash infra/scripts/smoke.sh` from a tailnet client.
5. Mobile Settings → Force reconnect.

If 1–3 are healthy but the mobile client is stuck, suspect the
client-side wire layer (clear browser storage or re-onboard).
