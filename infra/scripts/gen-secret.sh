#!/usr/bin/env bash
# Print a NOMADDEV_JWT_SECRET=… line backed by 48 bytes of /dev/urandom
# (>= the 32-byte minimum enforced by internal/config.MinSecretBytes).
# Usage:
#   infra/scripts/gen-secret.sh           # prints to stdout
#   infra/scripts/gen-secret.sh -raw      # prints just the secret value
set -euo pipefail

mode="${1:-line}"
secret="$(head -c 48 /dev/urandom | base64 | tr -d '\n')"

case "${mode}" in
    -raw|--raw)
        printf '%s\n' "${secret}"
        ;;
    *)
        printf 'NOMADDEV_JWT_SECRET=%s\n' "${secret}"
        ;;
esac
