# Data handling

This document inventories every piece of data the orchestrator touches:
what's persisted, where, for how long, and what leaves the host. Use
it to answer "is X retained?" and "what does my regulator need to
know?" without reading the code.

## On-disk persistence

All persistent state lives under `/var/lib/nomaddev` (overridable via
`NOMADDEV_*_PATH` env vars). Files are owned by the `nomaddev` user
with mode 0600 / 0640; the directory is 0750.

| File | What's in it | How long it sticks |
|---|---|---|
| `sessions.db` | Per-SID replay buffer (last N envelopes per session) for reconnect-resume | Ring-buffer bounded by `NOMADDEV_SESSION_BUFFER_SIZE` (256) and `NOMADDEV_SESSION_MAX_BYTES` (1 MiB); idle sessions reaped by the janitor after `NOMADDEV_SESSION_IDLE_TTL` (30m) |
| `history.db` | Conversation turns shown to the LLM (user text + assistant text + tool calls/results) | Per-SID; pruned when the session is reset via `user.command{reset_history}` (see `docs/operations.md`) |
| `revocations.db` | JTI revocation list for /auth/revoke + refresh-token rotation | Entry kept until the token's natural expiry; janitor runs every `NOMADDEV_AUTH_REVOCATION_JANITOR_INTERVAL` (5m) |
| `work/` | Sandbox bind-mounted workspace, per-session subdir when 10.2 is on | Operator-managed; not pruned automatically. `rm -rf` between runs to wipe |
| `audit.log` | JSON-Lines security audit stream (Phase 8.5) when `NOMADDEV_AUDIT_BACKEND=file` | Append-only; rotate via logrotate (`docs/operations.md`) |
| `backups/` | Daily SQLite snapshots (Phase 8.10) | `NOMADDEV_BACKUP_RETENTION_DAYS` (14) |

What's **not** persisted on disk:
- The JWT signing secret (`NOMADDEV_JWT_SECRET`) — lives in `/etc/nomaddev/env` only.
- The GitHub PAT (`NOMADDEV_GITHUB_TOKEN`) — same.
- The Gemini API key (`NOMADDEV_GEMINI_API_KEY`) — same.

## What leaves the host

The orchestrator makes outbound connections in three places. Each is
documented + optional.

### Gemini API (when `NOMADDEV_MIDDLEWARE_RUNTIME=gemini`)

The user's intent text, the rolling conversation window (last
`NOMADDEV_HISTORY_WINDOW_TURNS` turns, default 20), and the tool
catalogue's name+schema are POSTed to `generativelanguage.googleapis.com`
per user.intent envelope. Google's privacy posture for the Gemini API
is outside this project's scope; see
[Google's data-use policy for Gemini API](https://ai.google.dev/gemini-api/terms).

To opt out: set `NOMADDEV_MIDDLEWARE_RUNTIME=mock` (test stub) or
`=none` (returns error{not_implemented} for `user.intent`).

### GitHub API (when `NOMADDEV_GITHUB_TOKEN` is configured)

The orchestrator spawns `github-mcp-server` as a subprocess; that
binary makes requests against `api.github.com` (or the configured
GHES `NOMADDEV_GITHUB_HOST`). Every github_* tool call leaks:
- The tool arguments (file path, repo/owner, PR body, etc.)
- The PAT (in the `Authorization` header)
- The response (PR/issue/file contents)

The PAT scope is the blast radius. Recommend fine-grained PATs scoped
to a single repo when possible; see `docs/github.md`.

To opt out: leave `NOMADDEV_GITHUB_TOKEN` empty. The orchestrator
boots without any github_* tools.

### Tailscale coordination

The Tailscale daemon (separate process) maintains its own connection
to Tailscale's coordination server. NomadDev's orchestrator itself
doesn't talk to Tailscale — it listens on the tailnet IP that
Tailscale provides locally. See
[Tailscale's data policy](https://tailscale.com/security/data).

## Audit-trail content

The Phase 8.5 audit log (events: `ws.connect`, `ws.auth_failed`,
`auth.refresh`, `auth.revoke`, `approval.granted`, `approval.denied`)
records:

- `sub` (the JWT subject — operator identity)
- `sid` (session id)
- `remote` (the TCP remote-addr at handshake)
- `jti` (the token's unique id)
- `tool` (for approval events)
- `message` (free-form: rejection reason for auth failures, deny
  reason from the operator for `approval.denied`)

What it does **not** contain:
- The script content of a `command.request` (Phase 8.5 was security
  events only — see `internal/audit/audit.go` for the schema).
- Tool result bodies.
- Bearer tokens (the JTI identifies the token; the token bytes never
  hit the audit stream).

## Wire redaction

Approval-card envelopes (the operator-visible record of "approve this
tool call?") are redacted before they're shipped to the SPA, and
the same redaction runs before the envelope hits the SQLite replay
buffer:

- Argument values for keys matching the sensitive-key list
  (`token`, `password`, `secret`, `api_key`, `credential`, …) are
  replaced with `[REDACTED]` — see `internal/event/redact.go`.
- Inline-script secret assignments (Phase 10.1):
  `export NAME=value` shapes where NAME matches the same list also
  get the value masked.
- String values longer than 4096 bytes are truncated on the wire (the
  full value still reaches the dispatch path; display only).

What the redactor **doesn't** catch:
- Secrets embedded in free-form prose (chat history, PR descriptions).
  The model can choose to type a secret into a `user.intent` text
  field; that goes to Gemini in plain text and lands in `history.db`.
  Operators with a regulatory requirement should consider running
  the orchestrator with `NOMADDEV_MIDDLEWARE_RUNTIME=none` and the
  legacy command.request path only.

## Retention policy summary

| Surface | Default retention | Operator override |
|---|---|---|
| Session replay buffer | 30m idle TTL, 256-event ring | `NOMADDEV_SESSION_*` |
| History (LLM context) | Per-session, cleared on `reset_history` | manual `sqlite3 history.db 'DELETE FROM turns WHERE sid = …'` |
| Audit log | Append-only forever | `logrotate` (see `docs/operations.md`) |
| Backup snapshots | 14 days | `NOMADDEV_BACKUP_RETENTION_DAYS` |
| Workspace bind mount | Never (operator-managed) | Manual `rm -rf` |
| Revocation list | Until token expiry | `NOMADDEV_AUTH_REVOCATION_JANITOR_INTERVAL` |

## Deletion

To wipe all user state on a host:

```sh
sudo systemctl stop nomaddev-orchestrator nomaddev-backup.timer
sudo rm -rf /var/lib/nomaddev/{sessions.db,history.db,revocations.db,work,audit.log,backups}*
sudo systemctl start nomaddev-orchestrator
```

The orchestrator will recreate empty stores at the configured paths
on next boot.
