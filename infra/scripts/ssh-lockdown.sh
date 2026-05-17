#!/usr/bin/env bash
# NomadDev SSH lockdown — Phase 1.
#
# Idempotent script that hardens the public network interface AFTER the
# Tailscale mesh is up and `tailscale ssh` has been verified to work. Every
# destructive command is commented out with `# TODO:` so a careless
# `bash ssh-lockdown.sh` does nothing dangerous. Review each section and
# uncomment when you are ready.
#
# ORDERING IS CRITICAL. If you disable OpenSSH before confirming that
# `tailscale ssh` reaches this host, you will lock yourself out.
set -euo pipefail

echo "Phase 1.3a: Pre-flight — confirm Tailscale is operational"
# These checks are READ-ONLY and safe to run as-is.
if ! command -v tailscale >/dev/null 2>&1; then
    echo "  tailscale binary not found; run provision.sh step 1.2 first" >&2
    exit 1
fi
if ! tailscale status >/dev/null 2>&1; then
    echo "  tailscale is not running; bring it up with 'sudo tailscale up --ssh'" >&2
    exit 1
fi
TS_IP="$(tailscale ip -4 2>/dev/null | head -n1 || true)"
if [[ -z "${TS_IP}" ]]; then
    echo "  no tailscale IPv4 address assigned" >&2
    exit 1
fi
echo "  tailscale ip: ${TS_IP}"

echo "Phase 1.3b: Pre-flight — confirm tailscale ssh works"
# Print instructions rather than auto-test; we cannot impersonate the operator's
# client device from here.
cat <<EOF
  From your client device (phone or laptop on the tailnet), run:
      tailscale ssh <user>@${TS_IP}
  Confirm you can log in BEFORE running the destructive steps below.
EOF

echo "Phase 1.3c: Lock down ufw"
# TODO: sudo ufw default deny incoming
# TODO: sudo ufw default allow outgoing
# TODO: sudo ufw allow in on tailscale0
# TODO: sudo ufw --force enable
# TODO: sudo ufw status verbose

echo "Phase 1.3d: Disable OpenSSH on the public interface"
# Only run after 1.3a/b/c above succeed AND tailscale ssh is confirmed.
# TODO: sudo systemctl disable --now ssh
# TODO: sudo systemctl mask ssh

echo "Done (no-op). Review the TODOs above and uncomment when ready."
