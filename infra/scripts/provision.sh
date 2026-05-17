#!/usr/bin/env bash
# NomadDev VPS provisioning checklist — Phase 1.
#
# This file is intentionally NOT executable end-to-end. Every destructive
# command is commented out with `# TODO:` so a careless `bash provision.sh`
# does nothing dangerous. Review and run each section by hand.
set -euo pipefail

echo "Phase 1.1: System base"
# TODO: sudo apt-get update && sudo apt-get -y upgrade
# TODO: sudo apt-get install -y curl ufw

echo "Phase 1.2: Install Tailscale"
# TODO: curl -fsSL https://tailscale.com/install.sh | sh
# TODO: sudo tailscale up --ssh --hostname=nomaddev-host

echo "Phase 1.3: Lock down the public interface"
# Confirm `tailscale ssh` works BEFORE you run anything below.
# TODO: sudo ufw default deny incoming
# TODO: sudo ufw default allow outgoing
# TODO: sudo ufw allow in on tailscale0
# TODO: sudo ufw --force enable
# TODO: sudo systemctl disable --now ssh

echo "Done (no-op). Review the TODOs above and run them manually."
