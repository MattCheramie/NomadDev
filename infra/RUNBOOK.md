# NomadDev — Operator Runbook

First-deploy and incident runbook for the orchestrator + Tailscale mesh.
Reference; not auto-executed. Every destructive command in `infra/scripts/`
ships commented out with `# TODO:` — review and uncomment in place.

## First deploy

Ordering matters: never disable OpenSSH before `tailscale ssh` is confirmed
working.

1. **Provision host base.** Fresh Ubuntu 24.04 VPS.
   - Follow `infra/scripts/provision.sh` steps 1.1 and 1.2 (uncomment, run by
     hand). This installs Tailscale and brings the node up with `--ssh`.
   - From your phone/laptop on the tailnet, run `tailscale ssh user@host`.
     Confirm you can log in.

2. **Verify mesh.**
   ```sh
   bash infra/scripts/tailscale-verify.sh 8080
   ```
   All five checks must pass. If `tailscale0` doesn't have a `100.x.y.z`
   address, stop here and re-run `tailscale up`.

3. **Lock down public interface.**
   ```sh
   # Review the TODOs in infra/scripts/ssh-lockdown.sh, uncomment when ready.
   sudo bash infra/scripts/ssh-lockdown.sh
   ```
   The script's pre-flight refuses to proceed unless Tailscale is up. After
   uncommenting the ufw + systemctl lines, the public interface drops all
   inbound except what `tailscale0` lets through.

4. **Install the orchestrator.**
   - Choose one of the two deploy flavors (see PR 4 once shipped):
     - **systemd:** `sudo bash infra/scripts/install-systemd.sh`, then
       populate `/etc/nomaddev/env` from `.env.example`.
     - **Docker:** `docker compose up -d` from the repo root.
   - Either way, the binary listens on `:8080` and binds to the wildcard
     interface — the ufw rules from step 3 restrict it to `tailscale0`.

5. **Smoke test.**
   ```sh
   export NOMADDEV_JWT_SECRET="<the same secret the orchestrator is using>"
   URL=http://100.x.y.z:8080 bash infra/scripts/smoke.sh
   ```
   Three checks: `/healthz`, JWT mint, `command.request → command.result`.
   Exit code 0 ⇒ the daemon is live.

6. **Onboard the phone.**
   - Generate a QR: `go run ./scripts/qr-jwt -server-url http://100.x.y.z:8080 -sub matt -sid sess-1 -ttl 1h -out qr.png`
   - Open the deep link / scan the QR in the mobile SPA at
     `http://100.x.y.z:8080/`.

## Incident response

### Locked out (can't reach the host)
1. From the tailnet, try `tailscale ssh user@host`. If that works, the public
   firewall is doing its job — fix from inside.
2. If Tailscale itself is down on the host, log in via the cloud provider
   console (DigitalOcean / AWS recovery shell). Re-enable OpenSSH only as a
   temporary recovery measure: `sudo systemctl unmask ssh && sudo systemctl
   enable --now ssh`. Re-disable once Tailscale is back.

### `/healthz` returns 200 but `command.request` hangs
- Check `journalctl -u nomaddev-orchestrator -n 200` (systemd) or
  `docker compose logs orchestrator --tail 200` (Docker).
- Likely culprits:
  - `NOMADDEV_SANDBOX_RUNTIME=docker` without the Docker socket mounted.
  - `NOMADDEV_MIDDLEWARE_RUNTIME=gemini` without `NOMADDEV_GEMINI_API_KEY`.
- Drop back to the mock runners (`NOMADDEV_SANDBOX_RUNTIME=mock`,
  `NOMADDEV_MIDDLEWARE_RUNTIME=mock`) to bisect.

### Session bookmarks rolling over
- Symptom: clients see `session.stale` and lose feed history on reconnect.
- The ring buffer is bounded by `NOMADDEV_SESSION_BUFFER_SIZE` (count) and
  `NOMADDEV_SESSION_MAX_BYTES` (bytes). Bump either if your clients
  reconnect slowly.
- Persistent backend (`NOMADDEV_SESSION_BACKEND=sqlite`, shipping in PR 2)
  survives restarts but does not change the in-memory cap.

### Rotating the JWT secret
1. Generate a new secret: `head -c 48 /dev/urandom | base64`.
2. Update `/etc/nomaddev/env` (systemd) or `.env` (Docker).
3. Restart: `sudo systemctl restart nomaddev-orchestrator` or
   `docker compose restart orchestrator`.
4. Existing clients lose their JWTs and must re-onboard via QR. Issue new
   tokens with `go run ./scripts/gen-jwt -sub <user> -sid <sid> -ttl 1h`.

### Rollback after a bad release
- **systemd:** `sudo systemctl stop nomaddev-orchestrator`, replace
  `/usr/local/bin/orchestrator` with the previous binary, start.
- **Docker:** `docker compose down && docker compose up -d` after pinning
  the image tag to the last-known-good version in `docker-compose.yml`.
- Sessions: SQLite-backed sessions are forward-compatible — the schema is
  append-only. Conversation history likewise. Rolling back the binary
  doesn't corrupt either DB.
