#!/usr/bin/env bash
# NomadDev systemd installer — Phase 6 deploy.
#
# Non-destructive checklist that creates the runtime user, data dir, unit
# file, and env file for the orchestrator. Every system-modifying line is
# commented out with `# TODO:` so a careless `bash install-systemd.sh` does
# nothing dangerous. Review and uncomment in place.
#
# Assumes you have already produced /usr/local/bin/orchestrator (e.g. via
# `make build-full` then `sudo install -m 0755 bin/orchestrator
# /usr/local/bin/`).
set -euo pipefail

UNIT_SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/systemd/nomaddev-orchestrator.service"
UNIT_DST="/etc/systemd/system/nomaddev-orchestrator.service"
ENV_DIR="/etc/nomaddev"
ENV_FILE="${ENV_DIR}/env"
DATA_DIR="/var/lib/nomaddev"
USER_NAME="nomaddev"

echo "Phase 6.a: create runtime user '${USER_NAME}'"
# TODO: sudo useradd --system --no-create-home --shell /usr/sbin/nologin "${USER_NAME}"

echo "Phase 6.b: create data dir '${DATA_DIR}'"
# TODO: sudo install -d -o "${USER_NAME}" -g "${USER_NAME}" -m 0750 "${DATA_DIR}"
# TODO: sudo install -d -o "${USER_NAME}" -g "${USER_NAME}" -m 0750 "${DATA_DIR}/work"

echo "Phase 6.c: install env file at '${ENV_FILE}'"
# TODO: sudo install -d -m 0750 -o root -g root "${ENV_DIR}"
# TODO: sudo install -m 0640 -o root -g "${USER_NAME}" \
# TODO:     "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/.env.example" \
# TODO:     "${ENV_FILE}"
# Then edit ${ENV_FILE} and set at minimum:
#   NOMADDEV_JWT_SECRET=<from `head -c 48 /dev/urandom | base64`>

echo "Phase 6.d: install unit file"
echo "  source: ${UNIT_SRC}"
echo "  target: ${UNIT_DST}"
# TODO: sudo install -m 0644 -o root -g root "${UNIT_SRC}" "${UNIT_DST}"
# TODO: sudo systemctl daemon-reload

echo "Phase 6.e: enable + start"
# TODO: sudo systemctl enable --now nomaddev-orchestrator.service
# TODO: sudo systemctl status nomaddev-orchestrator.service --no-pager

echo
echo "Done (no-op). Review the TODOs above and uncomment when ready."
echo "Verify with: bash infra/scripts/smoke.sh"
