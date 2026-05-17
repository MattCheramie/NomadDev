# infra/ — Phase 1: Mesh & Foundation

Provisioning notes and scripts for the host VPS. Nothing here is Go.

## Overview

Phase 1 establishes secure, passwordless connectivity between the host VPS and
client devices via Tailscale. Once the mesh is up, public SSH is disabled —
the orchestrator binary from `cmd/orchestrator` is then reachable only over
the Tailscale interface (e.g. `100.x.y.z:8080`).

## Checklist (mirrors README Phase 1)

1. Provision Ubuntu 24.04 on the host VPS.
2. Install and configure Tailscale; bring the node up with `tailscale up --ssh`.
3. Verify ICMP + TCP traffic exclusively over the Tailscale `100.64.0.0/10` range.
4. Lock down `ufw`: deny all inbound on the public interface, allow only on
   `tailscale0`.
5. Disable the OpenSSH service (`systemctl disable --now ssh`) **after**
   confirming `tailscale ssh` works.

## ACL design (sketch)

Devices: `host-vps`, `phone`, `dev-laptop`.

```
"acls": [
  { "action": "accept", "src": ["phone", "dev-laptop"], "dst": ["host-vps:8080"] }
]
```

## Firewall plan

- `ufw default deny incoming`
- `ufw allow in on tailscale0`
- Tailscale UDP/41641 is handled by the daemon, no extra rule needed.

## Key rotation

Re-issue device auth keys via the Tailscale admin console every 90 days.
JWT signing secret (consumed by the orchestrator) rotates independently — see
`docs/auth.md`.

## Scripts

All scripts follow the same non-destructive convention: every destructive
command ships commented out with `# TODO:`. Review and uncomment in place.

- [`scripts/provision.sh`](./scripts/provision.sh) — base OS + Tailscale
  install checklist for a fresh Ubuntu 24.04 host.
- [`scripts/tailscale-verify.sh`](./scripts/tailscale-verify.sh) —
  read-only mesh verification (tailnet IP, `tailscale0` iface,
  orchestrator listener). Safe to re-run.
- [`scripts/ssh-lockdown.sh`](./scripts/ssh-lockdown.sh) — ufw hardening +
  OpenSSH disable. Has a pre-flight that refuses to proceed unless
  Tailscale is up.
- [`scripts/smoke.sh`](./scripts/smoke.sh) — end-to-end smoke against a
  running orchestrator: `/healthz`, JWT mint via `scripts/gen-jwt`, and a
  `command.request → command.result` round-trip via `cmd/wsclient`.

See [`RUNBOOK.md`](./RUNBOOK.md) for the ordered first-deploy walkthrough
and common incident-response recipes.
