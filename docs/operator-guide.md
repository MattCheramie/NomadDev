# NomadDev — Operator Guide

The single linear path from a bare VPS to a running, phone-connected
orchestrator. Follow the steps in order. Each step links to a deep-dive doc
where one exists; this guide is the index.

- **Transport security is Tailscale.** The orchestrator serves plain HTTP on
  `:8080`; the tailnet is the boundary. No TLS certificate is required. For
  HTTPS via a reverse proxy see [`auth.md`](./auth.md#tls-termination).
- **You do not build on the box.** Both quickstarts download prebuilt
  artifacts (a GHCR image or a release binary).

## What you need

- A fresh Ubuntu 24.04 VPS (4 GB RAM is comfortable — e.g. Hetzner CX22).
- A Tailscale account.
- For the Docker path: Docker + the `docker compose` v2 plugin.
- An LLM API key (Gemini, OpenAI, Anthropic, or DeepSeek) — optional; the
  default `mock` runtime works without one.

---

## Step 1 — Provision the host and join the tailnet

On the fresh VPS:

```sh
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --ssh
```

From a device already on your tailnet, confirm `tailscale ssh user@host`
logs in. Note the host's tailnet IPv4 (`tailscale ip -4`, a `100.x.y.z`
address) — it is used throughout.

Then clone the repo (only the scripts are used; nothing is compiled):

```sh
git clone https://github.com/MattCheramie/NomadDev.git
cd NomadDev
```

> Locking down the public interface (ufw + disabling public SSH) is covered
> in [`../infra/RUNBOOK.md`](../infra/RUNBOOK.md). Do it **after** confirming
> `tailscale ssh` works, never before — or you will lock yourself out.

## Step 2 — Deploy the orchestrator

Pick one path. Both are idempotent and safe to re-run.

**Docker** (recommended for 4 GB and smaller hosts):

```sh
sudo bash infra/scripts/quickstart-docker.sh
```

**systemd** (bare-metal, no Docker):

```sh
sudo bash infra/scripts/quickstart-systemd.sh
```

Each script runs preflight checks, generates `NOMADDEV_JWT_SECRET`, starts
the service, waits for `/healthz`, and runs the smoke test. On failure it
prints an ordered checklist of likely causes — follow it.

Configuration lives in an env file: `.env` (Docker) or `/etc/nomaddev/env`
(systemd). Every knob is documented in [`../.env.example`](../.env.example).
You rarely need to hand-edit it — Step 5 covers the in-app settings editor.

## Step 3 — Verify

The quickstart already ran the smoke test. To re-run it later:

```sh
# Docker:   reads .env automatically
# systemd:  set -a; source /etc/nomaddev/env; set +a
URL=http://100.x.y.z:8080 bash infra/scripts/smoke.sh
```

It checks `/healthz`, `/readyz`, and (when a Go toolchain is present) a full
`command.request → command.result` round-trip. Exit 0 means the daemon is
live.

## Step 4 — Onboard your phone

Mint an onboarding QR with the installed binary — no Go toolchain needed:

```sh
# systemd:
sudo bash -c 'set -a; . /etc/nomaddev/env; set +a; \
  orchestrator -mint-qr http://100.x.y.z:8080 -sub matt -sid sess-1 -ttl 1h'

# Docker:
docker compose exec orchestrator \
  orchestrator -mint-qr http://100.x.y.z:8080 -sub matt -sid sess-1 -ttl 1h
```

Open the SPA at `http://100.x.y.z:8080/` on the phone and scan the QR (the
token rides in the URL fragment, so it never lands in a log).

## Step 5 — Operate

### Change settings from the web UI

Open **Settings → Server configuration** in the SPA. Every orchestrator
setting is listed by category; edit any of them, then **Save & restart**.
Changes are persisted to a config-override file
(`NOMADDEV_CONFIG_OVERRIDE_PATH`, default
`/var/lib/nomaddev/config-override.json`) and applied on the restart that
follows. A variable hard-pinned in the env file still wins over the override.

The editor's API is gated by JWT scopes. Mint operator tokens with the
`config:read` scope to view settings and `config:write` to change them:

```sh
orchestrator -mint-qr http://100.x.y.z:8080 -sub matt \
  -scopes 'orchestrator:connect,config:read,config:write'
```

Tokens carrying no `config:` scope keep full access (backwards compatible).
Secrets (the JWT secret, API keys) are write-only — the API never returns
them. Changing the JWT secret signs every client out; see secret rotation
below.

### Backups

The systemd quickstart installs a daily SQLite backup timer
(`nomaddev-backup.timer` → `${DATA_DIR}/backups`, 14-day retention). Check it
with `systemctl list-timers nomaddev-backup.timer`. On Docker, run
`infra/scripts/nomaddev-backup.sh` from a host cron job against the
bind-mounted volume.

### Rotate the JWT secret

1. Set `NOMADDEV_JWT_PREV_SECRETS` to the current secret (a grace window so
   live tokens keep verifying), then set `NOMADDEV_JWT_SECRET` to a new value
   (`head -c 48 /dev/urandom | base64`). Restart.
2. After the longest token TTL has elapsed, clear
   `NOMADDEV_JWT_PREV_SECRETS`. Restart.

This can be done from the settings editor, but doing it in one step (without
the `PREV_SECRETS` grace window) invalidates every live token immediately.

### Logs and monitoring

- systemd: `journalctl -u nomaddev-orchestrator -f`
- Docker: `docker compose logs -f orchestrator`
- Metrics: `http://100.x.y.z:8080/metrics` (Prometheus).
- The audit log defaults to stderr; set `NOMADDEV_AUDIT_BACKEND=file` to
  write `/var/lib/nomaddev/audit.log` (the systemd quickstart installs a
  logrotate rule for it). See [`operations.md`](./operations.md).

### Troubleshooting and incident response

[`../infra/RUNBOOK.md`](../infra/RUNBOOK.md) is the incident annex: locked
out of the host, `/healthz` up but tool calls hanging, session-bookmark
rollover, and rollback after a bad release.

---

## Deep-dive references

| Topic | Doc |
|-------|-----|
| Every `NOMADDEV_*` setting | [`../.env.example`](../.env.example) |
| Auth, JWT scopes, TLS termination | [`auth.md`](./auth.md) |
| Sandbox runtime and threat model | [`sandbox.md`](./sandbox.md) |
| GitHub MCP integration | [`github.md`](./github.md) |
| Metrics, persistence, migrations | [`operations.md`](./operations.md) |
| Incident response | [`../infra/RUNBOOK.md`](../infra/RUNBOOK.md) |
| Mesh provisioning and ACLs | [`../infra/README.md`](../infra/README.md) |
