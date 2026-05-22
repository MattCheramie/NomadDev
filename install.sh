#!/usr/bin/env bash
# NomadDev one-liner VPS installer.
#
# Bootstraps an entire NomadDev deployment on a fresh Ubuntu VPS with a
# single command:
#
#   curl -fsSL https://raw.githubusercontent.com/MattCheramie/NomadDev/main/install.sh | sudo bash
#
# It installs the missing prerequisites (base packages, Tailscale, and —
# for the Docker path — Docker Engine), clones the repo to /opt/nomaddev,
# then hands off to the existing quickstart script, which downloads the
# prebuilt artifact, generates the JWT secret, starts the service, and runs
# the smoke test. Nothing is compiled on the box.
#
# Idempotent: safe to re-run — it updates the clone and re-runs the
# quickstart. The deploy paths themselves live in infra/scripts/.
#
# Usage:
#   sudo bash install.sh [--docker|--systemd] [--tailscale-authkey KEY] [--ref REF]
#
# When piped from curl, pass flags after `-s --`:
#   curl -fsSL https://raw.githubusercontent.com/MattCheramie/NomadDev/main/install.sh \
#       | sudo bash -s -- --docker
#
# Env overrides:
#   NOMADDEV_REF           git ref (branch/tag) to install (default: main).
#   NOMADDEV_REPO          GitHub owner/repo (default: MattCheramie/NomadDev).
#   NOMADDEV_INSTALL_DIR   clone destination (default: /opt/nomaddev).
#   NOMADDEV_BIND_ADDR     passed through to the quickstart (host iface for :8080).
set -euo pipefail

REPO="${NOMADDEV_REPO:-MattCheramie/NomadDev}"
REF="${NOMADDEV_REF:-main}"
INSTALL_DIR="${NOMADDEV_INSTALL_DIR:-/opt/nomaddev}"
MODE=""
TS_AUTHKEY=""

note() { printf '[install] %s\n' "$*"; }
fail() { printf '[install] ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'EOF'
NomadDev one-liner VPS installer.

Usage:
  sudo bash install.sh [options]

When piped from curl, pass flags after `-s --`:
  curl -fsSL https://raw.githubusercontent.com/MattCheramie/NomadDev/main/install.sh \
      | sudo bash -s -- --docker

Options:
  --docker                Force the Docker / GHCR deploy path.
  --systemd               Force the systemd binary deploy path.
                          (default: auto-detect — Docker if it is already
                          installed, otherwise systemd)
  --tailscale-authkey KEY Join the tailnet non-interactively with auth KEY.
  --ref REF               Git ref (branch or tag) to install (default: main).
  -h, --help              Show this help and exit.

Env overrides: NOMADDEV_REF, NOMADDEV_REPO, NOMADDEV_INSTALL_DIR,
NOMADDEV_BIND_ADDR.
EOF
}

# ------------------------------------------------------------------- flags
while [[ $# -gt 0 ]]; do
    case "$1" in
        --docker)  MODE="docker";  shift ;;
        --systemd) MODE="systemd"; shift ;;
        --tailscale-authkey)
            [[ $# -ge 2 ]] || fail "--tailscale-authkey needs a value"
            TS_AUTHKEY="$2"; shift 2 ;;
        --tailscale-authkey=*)
            TS_AUTHKEY="${1#*=}"; shift ;;
        --ref)
            [[ $# -ge 2 ]] || fail "--ref needs a value"
            REF="$2"; shift 2 ;;
        --ref=*)
            REF="${1#*=}"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) fail "unknown argument: $1 (try --help)" ;;
    esac
done

# --------------------------------------------------------------- preflight
[[ "$(id -u)" -eq 0 ]] || fail "must run as root (sudo bash install.sh)"
command -v systemctl >/dev/null 2>&1 || fail "systemctl not found; NomadDev requires a systemd host"
if ! command -v apt-get >/dev/null 2>&1; then
    fail "apt-get not found. This installer auto-installs prerequisites on
   Debian/Ubuntu (the project's supported VPS target). On another distro,
   install git + curl + Docker yourself, then run the matching script in
   infra/scripts/quickstart-<mode>.sh from a clone."
fi

case "$(uname -m)" in
    x86_64|aarch64) ;;
    *) fail "unsupported architecture $(uname -m); releases publish only amd64/arm64" ;;
esac

# ----------------------------------------------------------- base packages
# Only touch apt when something is actually missing — most VPS images ship
# curl/git already, and that lets a host with a broken third-party apt repo
# install cleanly. A failed `apt-get update` is a warning, not fatal: the
# three packages we need all live in the Ubuntu base repos.
missing_pkgs=()
command -v curl >/dev/null 2>&1            || missing_pkgs+=("curl")
dpkg -s ca-certificates >/dev/null 2>&1    || missing_pkgs+=("ca-certificates")
command -v git >/dev/null 2>&1             || missing_pkgs+=("git")
if [[ ${#missing_pkgs[@]} -gt 0 ]]; then
    note "installing base packages: ${missing_pkgs[*]}"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq || note "WARNING: apt-get update reported errors; continuing"
    apt-get install -y -qq "${missing_pkgs[@]}" \
        || fail "failed to install base packages: ${missing_pkgs[*]}"
else
    note "base packages already present (curl, ca-certificates, git)"
fi

# --------------------------------------------------------------- tailscale
# Tailscale is the transport-security boundary, but a failed install of it
# must not abort the whole deploy — the orchestrator still runs (bound to
# 127.0.0.1) and Tailscale can be added later. pipefail makes the `if`
# correctly observe a curl failure even though `sh` would exit 0 on it.
if ! command -v tailscale >/dev/null 2>&1; then
    note "installing Tailscale"
    if curl -fsSL https://tailscale.com/install.sh | sh; then
        note "Tailscale installed"
    else
        note "WARNING: Tailscale install failed — continuing without it. The"
        note "         orchestrator will bind to 127.0.0.1; install Tailscale"
        note "         later and re-run this script to expose it to the tailnet."
    fi
else
    note "Tailscale already installed"
fi

if [[ -n "${TS_AUTHKEY}" ]]; then
    if command -v tailscale >/dev/null 2>&1; then
        note "joining the tailnet with the supplied auth key"
        tailscale up --authkey="${TS_AUTHKEY}" --ssh
    else
        note "WARNING: --tailscale-authkey given but Tailscale is not installed; skipping"
    fi
fi

TS_READY="no"
if command -v tailscale >/dev/null 2>&1 && tailscale ip -4 >/dev/null 2>&1; then
    TS_READY="yes"
    note "tailnet IPv4: $(tailscale ip -4 | head -n1)"
else
    note "this host has not joined a tailnet yet — the orchestrator will bind to"
    note "127.0.0.1 until it does (see the reminder at the end of this run)"
fi

# ------------------------------------------------------------ clone / update
# git_retry runs a git command, retrying network failures with exponential
# backoff (2s, 4s, 8s, 16s) — matches the repo's git-ops convention.
git_retry() {
    local delay=2 attempt
    for attempt in 1 2 3 4 5; do
        if git "$@"; then
            return 0
        fi
        [[ ${attempt} -eq 5 ]] && return 1
        note "git operation failed (attempt ${attempt}); retrying in ${delay}s"
        sleep "${delay}"
        delay=$(( delay * 2 ))
    done
}

if [[ -e "${INSTALL_DIR}/.git" ]]; then
    note "updating existing clone at ${INSTALL_DIR} (ref: ${REF})"
    git_retry -C "${INSTALL_DIR}" fetch --depth 1 origin "${REF}" \
        || fail "git fetch failed after retries"
    # reset --hard to FETCH_HEAD makes the update deterministic. The
    # quickstart's .env is gitignored, so it survives untouched.
    git -C "${INSTALL_DIR}" reset --hard FETCH_HEAD \
        || fail "git reset to ${REF} failed"
elif [[ -e "${INSTALL_DIR}" ]]; then
    fail "${INSTALL_DIR} exists but is not a git checkout; move it aside and re-run"
else
    note "cloning ${REPO}@${REF} into ${INSTALL_DIR}"
    git_retry clone --depth 1 --branch "${REF}" \
        "https://github.com/${REPO}.git" "${INSTALL_DIR}" \
        || fail "git clone failed after retries"
fi

# ----------------------------------------------------------- mode selection
if [[ -z "${MODE}" ]]; then
    if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
        MODE="docker"
        note "auto-detected deploy mode: docker (Docker + compose v2 present)"
    else
        MODE="systemd"
        note "auto-detected deploy mode: systemd (Docker not detected)"
    fi
else
    note "deploy mode: ${MODE} (from flag)"
fi

# ------------------------------------------------------------ docker install
if [[ "${MODE}" == "docker" ]]; then
    if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
        note "installing Docker Engine + compose plugin (get.docker.com)"
        curl -fsSL https://get.docker.com | sh
        systemctl enable --now docker
        docker compose version >/dev/null 2>&1 \
            || fail "Docker installed but 'docker compose' is still unavailable"
    else
        note "Docker already installed"
    fi
fi

# ----------------------------------------------------------------- delegate
QUICKSTART="${INSTALL_DIR}/infra/scripts/quickstart-${MODE}.sh"
[[ -f "${QUICKSTART}" ]] || fail "quickstart script missing: ${QUICKSTART}"

note "handing off to quickstart-${MODE}.sh"
echo
bash "${QUICKSTART}"

# ----------------------------------------------------------------- reminder
if [[ "${TS_READY}" != "yes" ]]; then
    cat <<'EOF'

[install] REMINDER: this host has not joined a tailnet, so the orchestrator is
[install] bound to 127.0.0.1 and is not reachable from your phone yet. Run:
[install]     sudo tailscale up --ssh
[install] then re-run this installer so it rebinds to the host's tailnet IP.
EOF
fi
