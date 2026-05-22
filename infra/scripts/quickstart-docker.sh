#!/usr/bin/env bash
# NomadDev one-command Docker deploy.
#
# Pulls the prebuilt multi-arch image published by the release workflow
# (ghcr.io/mattcheramie/nomaddev:latest) and starts it via docker compose.
# Never builds locally — sidesteps the Go 1.25 + OOM concerns on small
# VPSes (Hetzner CX22, etc.). Fully executable (NOT # TODO:-commented);
# safe to re-run.
#
# Prerequisites: Docker + docker compose v2; Tailscale installed and
# `tailscale up` succeeded on the host. See infra/scripts/provision.sh.
#
# Usage:
#   sudo bash infra/scripts/quickstart-docker.sh
#
# Env vars:
#   NOMADDEV_BIND_ADDR  host iface to publish :8080 on. Auto-detected from
#                       `tailscale ip -4` when unset; falls back to 0.0.0.0
#                       if Tailscale isn't running (the operator should
#                       review this case before going to prod).
#   NOMADDEV_IMAGE      override the GHCR image tag.
#   NOMADDEV_ENV_FILE   path to .env (default: repo-root/.env).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

ENV_FILE="${NOMADDEV_ENV_FILE:-${REPO_ROOT}/.env}"

note() { printf '[quickstart] %s\n' "$*"; }
fail() { printf '[quickstart] ERROR: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- preflight
command -v docker >/dev/null 2>&1 || fail "docker not installed; see https://docs.docker.com/engine/install/"
if ! docker compose version >/dev/null 2>&1; then
    fail "docker compose v2 plugin not installed (or you're using legacy 'docker-compose')"
fi
command -v curl >/dev/null 2>&1 || fail "curl not installed"

# Warn on low disk — the image pull, container, and SQLite stores need room.
avail_mb="$(df -Pm /var/lib 2>/dev/null | awk 'NR==2 {print $4}')"
if [[ -n "${avail_mb}" && "${avail_mb}" -lt 500 ]]; then
    note "WARNING: only ${avail_mb} MiB free on /var/lib — the image pull may fail"
fi

# ------------------------------------------------------------- bind address
if [[ -z "${NOMADDEV_BIND_ADDR:-}" ]]; then
    if command -v tailscale >/dev/null 2>&1 && tailscale ip -4 >/dev/null 2>&1; then
        NOMADDEV_BIND_ADDR="$(tailscale ip -4 | head -n1)"
        note "auto-detected tailnet IPv4: ${NOMADDEV_BIND_ADDR}"
    else
        NOMADDEV_BIND_ADDR="127.0.0.1"
        note "Tailscale not detected; falling back to ${NOMADDEV_BIND_ADDR} (loopback)"
        note "Set NOMADDEV_BIND_ADDR=<your-tailnet-ip> to expose to the tailnet."
    fi
fi
export NOMADDEV_BIND_ADDR

# ------------------------------------------------------------------ env file
if [[ ! -f "${ENV_FILE}" ]]; then
    note "creating ${ENV_FILE} from .env.example"
    cp "${REPO_ROOT}/.env.example" "${ENV_FILE}"
fi

if grep -qE '^NOMADDEV_JWT_SECRET=changeme' "${ENV_FILE}" || \
   ! grep -qE '^NOMADDEV_JWT_SECRET=.+' "${ENV_FILE}"; then
    note "generating NOMADDEV_JWT_SECRET"
    SECRET_LINE="$(bash "${REPO_ROOT}/infra/scripts/gen-secret.sh")"
    # Replace existing placeholder line OR append. Use a temp file so we
    # don't truncate on sed failure.
    tmp="$(mktemp)"
    if grep -qE '^NOMADDEV_JWT_SECRET=' "${ENV_FILE}"; then
        awk -v line="${SECRET_LINE}" '
            /^NOMADDEV_JWT_SECRET=/ { print line; next } { print }
        ' "${ENV_FILE}" > "${tmp}"
    else
        cat "${ENV_FILE}" > "${tmp}"
        printf '%s\n' "${SECRET_LINE}" >> "${tmp}"
    fi
    mv "${tmp}" "${ENV_FILE}"
    chmod 600 "${ENV_FILE}"
fi

note "bind addr: ${NOMADDEV_BIND_ADDR}:8080"
note "env file:  ${ENV_FILE}"
note "image:     ${NOMADDEV_IMAGE:-ghcr.io/mattcheramie/nomaddev:latest}"

# ---------------------------------------------------------------- pull + up
note "pulling image"
docker compose --env-file "${ENV_FILE}" pull

note "starting container"
docker compose --env-file "${ENV_FILE}" up -d

# Wait briefly for /healthz to come up.
note "waiting for /healthz"
healthz="http://${NOMADDEV_BIND_ADDR}:8080/healthz"
for _ in $(seq 1 30); do
    if curl -fsS -o /dev/null "${healthz}" 2>/dev/null; then
        note "healthz ok"
        break
    fi
    sleep 1
done

if ! curl -fsS -o /dev/null "${healthz}" 2>/dev/null; then
    note "ERROR: /healthz did not come up after 30s. Check, in order:"
    note "  1. container state:   docker compose ps"
    note "  2. orchestrator log:  docker compose logs --tail=50 orchestrator"
    note "       'NOMADDEV_JWT_SECRET must be set'  -> the .env secret is missing or short"
    note "       'API key is empty'                -> a runtime is selected without its key"
    note "  3. port already bound: ss -ltnp | grep 8080"
    note "  4. slow image pull on a poor link      -> re-run this script"
    exit 1
fi

# ----------------------------------------------------------------- smoke run
# Source NOMADDEV_JWT_SECRET so smoke.sh can mint a token. The .env file is
# the source of truth; we just export the secret line into this shell.
set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

note "running smoke test"
URL="http://${NOMADDEV_BIND_ADDR}:8080" \
    bash "${REPO_ROOT}/infra/scripts/smoke.sh"

cat <<EOF

[quickstart] DONE.
  Orchestrator:  http://${NOMADDEV_BIND_ADDR}:8080
  Healthz:       http://${NOMADDEV_BIND_ADDR}:8080/healthz
  Metrics:       http://${NOMADDEV_BIND_ADDR}:8080/metrics
  SPA:           http://${NOMADDEV_BIND_ADDR}:8080/

  Mint a QR for the phone (the orchestrator binary renders it — no Go needed):
    docker compose exec orchestrator \\
        orchestrator -mint-qr http://${NOMADDEV_BIND_ADDR}:8080 -sub matt -sid sess-1 -ttl 1h
EOF
