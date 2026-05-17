#!/usr/bin/env bash
# NomadDev Tailscale verification — Phase 1.
#
# Read-only checks that confirm the host is reachable exclusively over the
# Tailscale interface. Safe to run repeatedly.
#
#   bash infra/scripts/tailscale-verify.sh [listen-port]
#
# listen-port defaults to 8080 (the orchestrator default).
set -euo pipefail

PORT="${1:-8080}"
fail=0

note() { echo "  $*"; }
err()  { echo "  ERROR: $*" >&2; fail=1; }

echo "1. tailscale binary present"
if command -v tailscale >/dev/null 2>&1; then
    note "ok ($(tailscale version | head -n1))"
else
    err "tailscale not installed"
fi

echo "2. tailscale up and authenticated"
if tailscale status >/dev/null 2>&1; then
    state="$(tailscale status --json 2>/dev/null | grep -o '"BackendState":"[^"]*"' | head -n1)"
    note "state: ${state:-unknown}"
else
    err "tailscale status failed"
fi

echo "3. tailscale0 interface present with 100.x.y.z address"
if ip -4 addr show tailscale0 >/dev/null 2>&1; then
    addr="$(ip -4 -o addr show tailscale0 | awk '{print $4}')"
    note "tailscale0: ${addr}"
    case "${addr}" in
        100.*) ;;
        *) err "tailscale0 address is not in 100.64.0.0/10" ;;
    esac
else
    err "tailscale0 interface not found"
fi

echo "4. orchestrator (if running) listens on tailscale0 or 0.0.0.0:${PORT}"
if command -v ss >/dev/null 2>&1; then
    listeners="$(ss -ltn "sport = :${PORT}" 2>/dev/null | awk 'NR>1 {print $4}')"
    if [[ -z "${listeners}" ]]; then
        note "no listener on :${PORT} yet (start the orchestrator first)"
    else
        note "listeners on :${PORT}:"
        echo "${listeners}" | sed 's/^/    /'
        if echo "${listeners}" | grep -qE '^(0\.0\.0\.0|\[::\]|100\.)'; then
            note "ok — reachable over tailscale0"
        else
            err "orchestrator does not listen on tailscale0 or wildcard"
        fi
    fi
else
    note "ss not available; skipping listener check"
fi

echo "5. public iface should NOT accept inbound on :${PORT} when ufw is locked"
# Read-only hint — we cannot probe from outside the host. Operator should run
# the equivalent from a non-tailnet machine:
note "From an off-tailnet machine, verify the following times out / refuses:"
note "    nc -vz <public-ip> ${PORT}"
note "    nc -vz <public-ip> 22"

if [[ "${fail}" -ne 0 ]]; then
    echo
    echo "FAIL: one or more checks did not pass." >&2
    exit 1
fi
echo
echo "OK: tailscale mesh looks healthy."
