#!/usr/bin/env bash
# NomadDev one-command systemd deploy.
#
# Downloads the prebuilt orchestrator binary for the host's architecture
# from the latest GitHub release, verifies the sha256, installs the
# systemd unit, and starts the service. Never compiles locally — avoids
# the Go 1.25 + npm install footprint that doesn't fit on small VPSes
# (Hetzner CX22, etc.).
#
# Fully executable (NOT # TODO:-commented). Idempotent — safe to re-run.
#
# Prerequisites: a Linux host with systemd; Tailscale installed; root
# (the script needs to create a user, /var/lib/nomaddev, /etc/nomaddev/,
# and install to /usr/local/bin).
#
# Usage:
#   sudo bash infra/scripts/quickstart-systemd.sh [version-tag]
#
# Env vars:
#   NOMADDEV_RELEASE     release tag to fetch (default: latest).
#   NOMADDEV_REPO        GitHub owner/repo (default: MattCheramie/NomadDev).
#   NOMADDEV_BIND_ADDR   informational only — used in the final message.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

USER_NAME="nomaddev"
DATA_DIR="/var/lib/nomaddev"
ENV_DIR="/etc/nomaddev"
ENV_FILE="${ENV_DIR}/env"
BIN_DST="/usr/local/bin/orchestrator"
BACKUP_BIN_DST="/usr/local/bin/nomaddev-backup"
UNIT_SRC="${REPO_ROOT}/infra/systemd/nomaddev-orchestrator.service"
UNIT_DST="/etc/systemd/system/nomaddev-orchestrator.service"
BACKUP_SVC_SRC="${REPO_ROOT}/infra/systemd/nomaddev-backup.service"
BACKUP_SVC_DST="/etc/systemd/system/nomaddev-backup.service"
BACKUP_TIMER_SRC="${REPO_ROOT}/infra/systemd/nomaddev-backup.timer"
BACKUP_TIMER_DST="/etc/systemd/system/nomaddev-backup.timer"

RELEASE="${1:-${NOMADDEV_RELEASE:-latest}}"
REPO="${NOMADDEV_REPO:-MattCheramie/NomadDev}"

note() { printf '[quickstart] %s\n' "$*"; }
fail() { printf '[quickstart] ERROR: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- preflight
if [[ "$(id -u)" -ne 0 ]]; then
    fail "must run as root (sudo bash $0)"
fi
command -v systemctl >/dev/null 2>&1 || fail "systemctl not found; this script requires systemd"
command -v curl >/dev/null 2>&1 || fail "curl not installed"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum not found"

case "$(uname -m)" in
    x86_64)  ARCH="amd64" ;;
    aarch64) ARCH="arm64" ;;
    *) fail "unsupported architecture: $(uname -m); release publishes only amd64/arm64" ;;
esac
note "host arch: ${ARCH}"

# Warn on low disk — the binary, SQLite stores, and daily backups need room.
avail_mb="$(df -Pm /var 2>/dev/null | awk 'NR==2 {print $4}')"
if [[ -n "${avail_mb}" && "${avail_mb}" -lt 500 ]]; then
    note "WARNING: only ${avail_mb} MiB free on /var — SQLite stores and backups need room"
fi

# ---------------------------------------------------------------- user + dirs
if ! getent passwd "${USER_NAME}" >/dev/null 2>&1; then
    note "creating user ${USER_NAME}"
    useradd --system --no-create-home --shell /usr/sbin/nologin "${USER_NAME}"
else
    note "user ${USER_NAME} already exists"
fi

install -d -o "${USER_NAME}" -g "${USER_NAME}" -m 0750 "${DATA_DIR}"
install -d -o "${USER_NAME}" -g "${USER_NAME}" -m 0750 "${DATA_DIR}/work"
install -d -o root -g root -m 0750 "${ENV_DIR}"

# ---------------------------------------------------------------- env file
if [[ ! -f "${ENV_FILE}" ]]; then
    note "seeding ${ENV_FILE} from .env.example"
    install -m 0640 -o root -g "${USER_NAME}" "${REPO_ROOT}/.env.example" "${ENV_FILE}"
fi

if grep -qE '^NOMADDEV_JWT_SECRET=changeme' "${ENV_FILE}" || \
   ! grep -qE '^NOMADDEV_JWT_SECRET=.+' "${ENV_FILE}"; then
    note "generating NOMADDEV_JWT_SECRET in ${ENV_FILE}"
    SECRET_LINE="$(bash "${REPO_ROOT}/infra/scripts/gen-secret.sh")"
    tmp="$(mktemp)"
    if grep -qE '^NOMADDEV_JWT_SECRET=' "${ENV_FILE}"; then
        awk -v line="${SECRET_LINE}" '
            /^NOMADDEV_JWT_SECRET=/ { print line; next } { print }
        ' "${ENV_FILE}" > "${tmp}"
    else
        cat "${ENV_FILE}" > "${tmp}"
        printf '%s\n' "${SECRET_LINE}" >> "${tmp}"
    fi
    install -m 0640 -o root -g "${USER_NAME}" "${tmp}" "${ENV_FILE}"
    rm -f "${tmp}"
fi

# ---------------------------------------------------------------- binary
ASSET="orchestrator-linux-${ARCH}"
if [[ "${RELEASE}" == "latest" ]]; then
    BASE="https://github.com/${REPO}/releases/latest/download"
else
    BASE="https://github.com/${REPO}/releases/download/${RELEASE}"
fi

note "downloading ${BASE}/${ASSET}"
tmpbin="$(mktemp)"
curl -fsSL -o "${tmpbin}" "${BASE}/${ASSET}" \
    || fail "download failed; check that release ${RELEASE} exists and publishes ${ASSET}"

note "verifying sha256"
tmpsum="$(mktemp)"
curl -fsSL -o "${tmpsum}" "${BASE}/${ASSET}.sha256" \
    || fail "sha256 download failed"

expected="$(awk '{print $1}' "${tmpsum}")"
actual="$(sha256sum "${tmpbin}" | awk '{print $1}')"
if [[ "${expected}" != "${actual}" ]]; then
    fail "sha256 mismatch: expected ${expected}, got ${actual}"
fi

install -m 0755 -o root -g root "${tmpbin}" "${BIN_DST}"
rm -f "${tmpbin}" "${tmpsum}"
note "installed ${BIN_DST} ($("${BIN_DST}" -version 2>/dev/null || echo dev))"

# ---------------------------------------------------------------- github-mcp-server
# The orchestrator binary's GitHub MCP integration spawns the upstream
# github-mcp-server as a subprocess (see docs/github.md). Install it only
# when the operator has configured a token — keeps the deploy footprint
# small for users who don't want the feature.
GHMCP_BIN_DST="/usr/local/bin/github-mcp-server"
# Keep this pinned version in step with GITHUB_MCP_VERSION in the Dockerfile.
GHMCP_VERSION="${NOMADDEV_GITHUB_MCP_VERSION:-v1.0.4}"
NEED_GHMCP="no"
GHMCP_FAILED="no"
if grep -qE '^NOMADDEV_GITHUB_TOKEN=.+' "${ENV_FILE}" 2>/dev/null; then
    # Token is set to something non-empty.
    if ! grep -qE '^NOMADDEV_GITHUB_TOKEN=$' "${ENV_FILE}" 2>/dev/null; then
        NEED_GHMCP="yes"
    fi
fi

if [[ "${NEED_GHMCP}" == "yes" ]]; then
    if [[ -x "${GHMCP_BIN_DST}" ]]; then
        note "github-mcp-server already installed at ${GHMCP_BIN_DST}"
    else
        # Upstream's GoReleaser maps amd64 → x86_64 in the asset name;
        # arm64 stays arm64. Keep the local ARCH var (used in the
        # orchestrator-binary download earlier in this script) untouched.
        case "${ARCH}" in
            amd64) GHMCP_ARCH="x86_64" ;;
            arm64) GHMCP_ARCH="arm64" ;;
            *) GHMCP_ARCH="${ARCH}" ;;
        esac
        GHMCP_ASSET="github-mcp-server_Linux_${GHMCP_ARCH}.tar.gz"
        GHMCP_URL="https://github.com/github/github-mcp-server/releases/download/${GHMCP_VERSION}/${GHMCP_ASSET}"
        note "downloading github-mcp-server ${GHMCP_VERSION} from ${GHMCP_URL}"
        tmpdir="$(mktemp -d)"
        if curl -fsSL -o "${tmpdir}/${GHMCP_ASSET}" "${GHMCP_URL}"; then
            tar -xzf "${tmpdir}/${GHMCP_ASSET}" -C "${tmpdir}" github-mcp-server \
                || fail "could not extract github-mcp-server from ${GHMCP_ASSET}"
            install -m 0755 -o root -g root "${tmpdir}/github-mcp-server" "${GHMCP_BIN_DST}"
            note "installed ${GHMCP_BIN_DST}"
        else
            GHMCP_FAILED="yes"
            note "WARNING: github-mcp-server download failed. The orchestrator will boot,"
            note "         but every github_* tool fails until the binary is installed."
            note "         Fix: install it manually (see docs/github.md) and re-run this script."
        fi
        rm -rf "${tmpdir}"
    fi
else
    note "NOMADDEV_GITHUB_TOKEN unset — skipping github-mcp-server install"
fi

# ---------------------------------------------------------------- backups
# Daily SQLite snapshot via the systemd timer. sqlite3 is the only
# runtime dep; install it if missing so the timer can fire without
# operator intervention on the first day.
if ! command -v sqlite3 >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then
        note "installing sqlite3 for the backup timer"
        DEBIAN_FRONTEND=noninteractive apt-get update -qq
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq sqlite3
    else
        note "WARNING: sqlite3 not found and no apt-get available — install it manually"
        note "         or the daily backup timer will fail silently"
    fi
fi
note "installing ${BACKUP_BIN_DST}"
install -m 0755 -o root -g root "${REPO_ROOT}/infra/scripts/nomaddev-backup.sh" "${BACKUP_BIN_DST}"

# ---------------------------------------------------------------- logrotate
# The audit log only lands on disk when NOMADDEV_AUDIT_BACKEND=file, but
# install the rule unconditionally (missingok) so switching the backend
# later does not silently grow an un-rotated log until it fills the disk.
# postrotate sends SIGHUP — the orchestrator reopens the audit file on HUP.
if command -v logrotate >/dev/null 2>&1; then
    note "installing logrotate fragment for the audit log"
    cat > /etc/logrotate.d/nomaddev <<'LOGROTATE'
/var/lib/nomaddev/audit.log {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    su nomaddev nomaddev
    postrotate
        systemctl kill -s HUP nomaddev-orchestrator.service 2>/dev/null || true
    endscript
}
LOGROTATE
else
    note "WARNING: logrotate not installed — if you set NOMADDEV_AUDIT_BACKEND=file,"
    note "         install logrotate or audit.log will grow unbounded."
fi

# ---------------------------------------------------------------- systemd
note "installing unit ${UNIT_DST}"
install -m 0644 -o root -g root "${UNIT_SRC}" "${UNIT_DST}"
note "installing backup service + timer"
install -m 0644 -o root -g root "${BACKUP_SVC_SRC}" "${BACKUP_SVC_DST}"
install -m 0644 -o root -g root "${BACKUP_TIMER_SRC}" "${BACKUP_TIMER_DST}"
systemctl daemon-reload
systemctl enable --now nomaddev-orchestrator.service
# Timer enables the daily oneshot run; on first install we don't
# `--now` it because the orchestrator may not have created its DBs yet.
systemctl enable nomaddev-backup.timer
systemctl start nomaddev-backup.timer

# Give it a moment to bind.
note "waiting for /healthz"
for _ in $(seq 1 15); do
    if curl -fsS -o /dev/null http://127.0.0.1:8080/healthz 2>/dev/null; then
        note "healthz ok"
        break
    fi
    sleep 1
done

if ! curl -fsS -o /dev/null http://127.0.0.1:8080/healthz 2>/dev/null; then
    note "ERROR: /healthz did not come up. Check, in order:"
    note "  1. service state:  systemctl status nomaddev-orchestrator"
    note "  2. logs:           journalctl -u nomaddev-orchestrator -n 50"
    note "       'NOMADDEV_JWT_SECRET must be set' -> the secret in ${ENV_FILE} is missing or short"
    note "       'API key is empty'               -> a runtime is selected without its key"
    note "  3. port already bound: ss -ltnp | grep 8080"
    fail "/healthz did not come up after 15s"
fi

# ---------------------------------------------------------------- smoke
note "running smoke test"
# Source the env file so smoke.sh can mint a JWT with the same secret.
set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

URL="http://127.0.0.1:8080" bash "${REPO_ROOT}/infra/scripts/smoke.sh"

# ---------------------------------------------------------------- done
TAILNET_IP="${NOMADDEV_BIND_ADDR:-}"
if [[ -z "${TAILNET_IP}" ]] && command -v tailscale >/dev/null 2>&1; then
    TAILNET_IP="$(tailscale ip -4 2>/dev/null | head -n1 || true)"
fi

cat <<EOF

[quickstart] DONE.
  systemctl status:  systemctl status nomaddev-orchestrator
  logs:              journalctl -u nomaddev-orchestrator -f
  Healthz:           http://127.0.0.1:8080/healthz
  Readyz:            http://127.0.0.1:8080/readyz
  Metrics:           http://127.0.0.1:8080/metrics
  Backups:           systemctl list-timers nomaddev-backup.timer
                     (daily snapshots → ${DATA_DIR}/backups, 14d retention)
EOF

if [[ "${GHMCP_FAILED}" == "yes" ]]; then
    cat <<EOF

[quickstart] NOTE: github-mcp-server was NOT installed (its download failed).
             github_* tools stay disabled until you install it manually —
             see docs/github.md — and re-run this script.
EOF
fi

if [[ -n "${TAILNET_IP}" ]]; then
    cat <<EOF
  SPA on tailnet:    http://${TAILNET_IP}:8080/

  Mint a QR for the phone (the installed binary renders it — no Go needed):
    sudo bash -c 'set -a; . ${ENV_FILE}; set +a; \\
      orchestrator -mint-qr http://${TAILNET_IP}:8080 -sub matt -sid sess-1 -ttl 1h'
EOF
fi
