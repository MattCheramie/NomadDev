#!/usr/bin/env bash
# NomadDev end-to-end smoke test.
#
# Verifies a running orchestrator. The check has two tiers:
#   1. Always: GET /healthz (process up) and GET /readyz (dependencies ok).
#   2. When the Go toolchain is present: mint a JWT and drive a full
#      command.request -> command.result round-trip through /ws.
#
# Tier 2 is skipped — not failed — when `go` is unavailable, so the
# Go-free quickstart deploys still get a meaningful health check instead
# of aborting. Exit non-zero only on a real failure. Safe to run repeatedly.
#
#   URL=http://127.0.0.1:8080 bash infra/scripts/smoke.sh
#   URL=http://100.x.y.z:8080 bash infra/scripts/smoke.sh   # over Tailscale
#
# Env vars:
#   URL           orchestrator base URL (default http://127.0.0.1:8080)
#   SUB           JWT subject (default smoke)
#   SID           JWT session id (default smoke-$$ )
#   TIMEOUT       wsclient timeout (default 10s)
#   NOMADDEV_JWT_SECRET must be exported and match the running orchestrator
#                       (only needed for the tier-2 round-trip).
set -euo pipefail

URL="${URL:-http://127.0.0.1:8080}"
WS_URL="${URL/http:/ws:}"
WS_URL="${WS_URL/https:/wss:}/ws"
SUB="${SUB:-smoke}"
SID="${SID:-smoke-$$}"
TIMEOUT="${TIMEOUT:-10s}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

# fail prints a diagnosis with the most likely causes, then exits.
fail() {
    echo "smoke: FAIL — $1" >&2
    shift || true
    for hint in "$@"; do
        echo "  - ${hint}" >&2
    done
    exit 1
}

echo "smoke: 1/4 healthz"
status="$(curl -fsS -o /dev/null -w '%{http_code}' "${URL}/healthz" 2>/dev/null || echo 000)"
if [[ "${status}" != "200" ]]; then
    fail "GET /healthz returned ${status} (expected 200)" \
        "the orchestrator is not running, crashed on boot, or :8080 is bound by something else" \
        "check the logs: 'journalctl -u nomaddev-orchestrator -n 50' or 'docker compose logs orchestrator'" \
        "confirm the URL is reachable — a Tailscale ACL may be blocking ${URL}"
fi
echo "  ok (200)"

echo "smoke: 2/4 readyz"
ready="$(curl -fsS -w '\n%{http_code}' "${URL}/readyz" 2>/dev/null || echo $'\n000')"
ready_code="$(printf '%s' "${ready}" | tail -n1)"
if [[ "${ready_code}" != "200" ]]; then
    fail "GET /readyz returned ${ready_code} (expected 200)" \
        "the process is up but a dependency is unhealthy (usually a SQLite store under /var/lib/nomaddev)" \
        "body: $(printf '%s' "${ready}" | head -n1)"
fi
echo "  ok (200)"

if ! command -v go >/dev/null 2>&1; then
    echo "smoke: 3/4 round-trip — SKIPPED (no Go toolchain on this host)"
    echo "smoke: 4/4 round-trip — SKIPPED"
    echo
    echo "smoke: PASS (health checks only; install Go to exercise the full /ws round-trip)"
    exit 0
fi

if [[ -z "${NOMADDEV_JWT_SECRET:-}" ]]; then
    fail "NOMADDEV_JWT_SECRET is not set" \
        "the round-trip needs the orchestrator's signing secret to mint a JWT" \
        "on systemd: 'set -a; source /etc/nomaddev/env; set +a' then re-run"
fi

echo "smoke: 3/4 mint JWT (sub=${SUB} sid=${SID})"
TOKEN="$(go run ./scripts/gen-jwt -sub "${SUB}" -sid "${SID}" -ttl 5m)"
if [[ -z "${TOKEN}" ]]; then
    fail "gen-jwt produced an empty token" \
        "NOMADDEV_JWT_SECRET may be malformed (it must decode to >=32 bytes)"
fi
echo "  ok ($(echo "${TOKEN}" | head -c 16)...)"

echo "smoke: 4/4 command.request round-trip"
if ! go run ./cmd/wsclient \
    -url "${WS_URL}" -token "${TOKEN}" \
    -send command.request -script 'echo smoke-ok' \
    -disconnect-after command.result -timeout "${TIMEOUT}"; then
    fail "command.request did not return a command.result" \
        "if /healthz and /readyz passed, the orchestrator is up — suspect the JWT (secret mismatch) or the sandbox runtime" \
        "check the orchestrator logs for the ws.auth_failed or sandbox error around this timestamp"
fi
echo "  ok"

echo
echo "smoke: PASS"
