#!/usr/bin/env bash
# NomadDev end-to-end smoke test — Phase 1.
#
# Drives a real orchestrator round-trip:
#   1. mint a JWT via scripts/gen-jwt
#   2. curl /healthz
#   3. wsclient command.request → expect command.result
#
# Assumes the orchestrator is already running and reachable. Exit non-zero on
# any failure. Safe to run repeatedly.
#
#   URL=http://127.0.0.1:8080 bash infra/scripts/smoke.sh
#   URL=http://100.x.y.z:8080 bash infra/scripts/smoke.sh   # over Tailscale
#
# Env vars:
#   URL           orchestrator base URL (default http://127.0.0.1:8080)
#   SUB           JWT subject (default smoke)
#   SID           JWT session id (default smoke-$$ )
#   TIMEOUT       wsclient timeout (default 10s)
#   NOMADDEV_JWT_SECRET must be exported and match the running orchestrator.
set -euo pipefail

URL="${URL:-http://127.0.0.1:8080}"
WS_URL="${URL/http:/ws:}"
WS_URL="${WS_URL/https:/wss:}/ws"
SUB="${SUB:-smoke}"
SID="${SID:-smoke-$$}"
TIMEOUT="${TIMEOUT:-10s}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

if [[ -z "${NOMADDEV_JWT_SECRET:-}" ]]; then
    echo "smoke: NOMADDEV_JWT_SECRET is not set (must match the running orchestrator)" >&2
    exit 1
fi

echo "smoke: 1/3 healthz"
status="$(curl -fsS -o /dev/null -w '%{http_code}' "${URL}/healthz")"
if [[ "${status}" != "200" ]]; then
    echo "smoke: /healthz returned HTTP ${status}" >&2
    exit 1
fi
echo "  ok (200)"

echo "smoke: 2/3 mint JWT (sub=${SUB} sid=${SID})"
TOKEN="$(go run ./scripts/gen-jwt -sub "${SUB}" -sid "${SID}" -ttl 5m)"
if [[ -z "${TOKEN}" ]]; then
    echo "smoke: gen-jwt produced empty token" >&2
    exit 1
fi
echo "  ok ($(echo "${TOKEN}" | head -c 16)...)"

echo "smoke: 3/3 command.request round-trip"
go run ./cmd/wsclient \
    -url "${WS_URL}" -token "${TOKEN}" \
    -send command.request -script 'echo smoke-ok' \
    -disconnect-after command.result -timeout "${TIMEOUT}"
echo "  ok"

echo
echo "smoke: PASS"
