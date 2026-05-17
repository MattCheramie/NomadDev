# NomadDev — Operator Runbook

First-deploy and incident runbook for the orchestrator + Tailscale mesh.

> **No TLS certificate is required.** The orchestrator runs over plain
> HTTP on `:8080`; Tailscale is the transport-security boundary. See
> [`docs/auth.md#tls-termination`](../docs/auth.md#tls-termination) if
> you want HTTPS via a Caddy/nginx proxy.

## TL;DR — fast paths

Two fully-executable wrapper scripts cover the happy path on a fresh
Ubuntu 24.04 VPS. Both download prebuilt artifacts (GHCR image or
release binary), generate a JWT secret, install/start the service, and
run the smoke test. Re-runnable.

### Docker (recommended for Hetzner CX22 / 4 GB and below)
```sh
# 1. On a fresh box, install Tailscale and bring it up:
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --ssh

# 2. Clone the repo (only needed for the scripts; no build happens):
git clone https://github.com/MattCheramie/NomadDev.git
cd NomadDev

# 3. Deploy:
sudo bash infra/scripts/quickstart-docker.sh
```

### systemd (bare-metal, no Docker required)
```sh
# Steps 1 + 2 as above, then:
sudo bash infra/scripts/quickstart-systemd.sh
```

After either flow finishes, the script prints the QR-generation command
for onboarding a phone. The full step-by-step walkthrough below covers
ops-conscious operators who want to review every script before running
it; for those operators, every destructive command in `infra/scripts/`
ships commented out with `# TODO:`.

## First deploy (manual walkthrough)

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

4. **Test from the public IP, then lock down.** Before
   `ssh-lockdown.sh` runs, confirm the orchestrator binds correctly:
   ```sh
   # From your laptop (off-tailnet) — port 22 should answer, :8080 may
   # answer if you started the orchestrator already:
   nc -vz <public-ip> 22
   nc -vz <public-ip> 8080
   ```
   Then lock down (step 3 above), and verify that the same `nc` calls
   now time out. If you skipped this verification and the lockdown
   silently broke connectivity, see "Locked out" below.

5. **Install the orchestrator.** Pick one:
   - **systemd:** `sudo bash infra/scripts/install-systemd.sh` (review
     each `# TODO:` and uncomment), or use the
     `quickstart-systemd.sh` wrapper above which automates the whole
     flow.
   - **Docker:** `docker compose up -d` from the repo root, or use the
     `quickstart-docker.sh` wrapper.
   Either way, the binary listens on `:8080` and binds to the wildcard
   interface — the ufw rules from step 3 restrict it to `tailscale0`.

6. **Smoke test.**
   ```sh
   export NOMADDEV_JWT_SECRET="<the same secret the orchestrator is using>"
   URL=http://100.x.y.z:8080 bash infra/scripts/smoke.sh
   ```
   Three checks: `/healthz`, JWT mint, `command.request → command.result`.
   Exit code 0 ⇒ the daemon is live.

7. **Onboard the phone.**
   - Generate a QR: `go run ./scripts/qr-jwt -server-url http://100.x.y.z:8080 -sub matt -sid sess-1 -ttl 1h -out qr.png`
   - Open the deep link / scan the QR in the mobile SPA at
     `http://100.x.y.z:8080/`.

## Cloud-provider notes

### Hetzner

Hetzner has two firewall layers, both default-allow on a fresh image.
After `ssh-lockdown.sh` closes ufw on the host, the orchestrator port
is still reachable from the public internet via Hetzner's edge unless
you also lock down at the cloud level:

1. **Hetzner Cloud Firewall** (web UI → Firewalls): create a firewall
   that allows inbound TCP 22 (only while you're still using public
   SSH), inbound UDP 41641 (Tailscale's NAT-traversal port), and
   denies everything else. Apply it to the server. After confirming
   Tailscale works, you can remove the TCP 22 rule.
2. **ufw on the host** (via `infra/scripts/ssh-lockdown.sh`).

Both layers are belt-and-suspenders; you want them. The host-side ufw
catches anything that gets past the cloud firewall (e.g. a misconfig);
the cloud firewall catches anything before it reaches the box.

Image / size guidance:
- **CX22 (4 GB, ~€4/mo)** is the cheapest plan that comfortably runs
  the orchestrator. Use the Docker quickstart so you never compile on
  the box.
- **CAX11 (4 GB, ARM, ~€3.79/mo)** also works — the prebuilt GHCR
  image is multi-arch (linux/arm64).
- Cloud-init is not used by these scripts; they assume an interactive
  ssh-then-run flow.

### IPv6

Hetzner assigns a /64 to every server, and Tailscale issues each node
a `fd7a:115c:…` v6 address. The orchestrator binds dual-stack on
`:8080` by default (Go's `net.Listen` behavior), so both
`http://[fd7a:…]:8080` and `http://100.x.y.z:8080` work from the
tailnet. `tailscale-verify.sh` prints both addresses; pick whichever
your client prefers when minting the QR.

### Other providers

DigitalOcean, Vultr, Linode: only ufw matters (no separate cloud
firewall by default). The Docker quickstart works on any provider that
ships `docker compose v2`; the systemd quickstart works on any
provider that ships `systemd` + the architecture's prebuilt binary
exists (amd64 or arm64).

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
