#!/usr/bin/env bash
# nomaddev-backup — daily SQLite snapshot for /var/lib/nomaddev.
#
# Uses `sqlite3 .backup` (the online backup API) so the orchestrator
# can keep serving while we copy pages. Each backup is integrity-checked
# before being kept; corrupt artifacts are deleted on the spot so we
# never silently retain a bad copy. Old archives are pruned per
# NOMADDEV_BACKUP_RETENTION_DAYS.
#
# Designed for the systemd path (timer-driven, runs as the nomaddev
# user). Operators on the Docker / Compose path can run this from a
# host cron job against the bind-mounted /var/lib/nomaddev volume.
#
# Usage:
#   nomaddev-backup               # back up + prune; idempotent
#   NOMADDEV_BACKUP_DIR=/mnt/...  # override location
#
# Env vars (all optional):
#   NOMADDEV_DATA_DIR              source of the live DBs (default /var/lib/nomaddev)
#   NOMADDEV_BACKUP_DIR            destination (default ${DATA_DIR}/backups)
#   NOMADDEV_BACKUP_RETENTION_DAYS keep this many days of archives (default 14)
set -euo pipefail

DATA_DIR="${NOMADDEV_DATA_DIR:-/var/lib/nomaddev}"
BACKUP_DIR="${NOMADDEV_BACKUP_DIR:-${DATA_DIR}/backups}"
RETENTION_DAYS="${NOMADDEV_BACKUP_RETENTION_DAYS:-14}"

# The three SQLite stores the orchestrator writes. Append to this
# array if a future phase adds another DB.
DBS=(sessions.db history.db revocations.db)

note() { printf '[nomaddev-backup] %s\n' "$*"; }
fail() { printf '[nomaddev-backup] ERROR: %s\n' "$*" >&2; exit 1; }

command -v sqlite3 >/dev/null 2>&1 || fail "sqlite3 not installed (apt install sqlite3)"
command -v gzip >/dev/null 2>&1 || fail "gzip not installed"

install -d -m 0700 "${BACKUP_DIR}"

# Pre-flight: a near-full disk yields a truncated, useless backup. Refuse
# rather than silently produce a bad archive.
avail_kb="$(df -Pk "${BACKUP_DIR}" 2>/dev/null | awk 'NR==2 {print $4}')"
if [[ -n "${avail_kb}" && "${avail_kb}" -lt 51200 ]]; then
    fail "only ${avail_kb} KiB free at ${BACKUP_DIR}; free space before backing up"
fi

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
backed_up=0

for db in "${DBS[@]}"; do
    src="${DATA_DIR}/${db}"
    if [[ ! -f "${src}" ]]; then
        note "skip ${db}: not present (orchestrator may not have created it yet)"
        continue
    fi
    out="${BACKUP_DIR}/${db%.db}.${stamp}.db"
    note "backing up ${src} -> ${out}"
    # Online backup; safe with concurrent writers.
    sqlite3 "${src}" ".backup '${out}'"

    # Verify the snapshot before we declare success. A corrupt page
    # in the source manifests here; deleting the bad copy stops it
    # from masking the next day's healthy backup.
    if ! sqlite3 "${out}" 'PRAGMA integrity_check' | grep -q '^ok$'; then
        rm -f "${out}"
        fail "integrity_check failed on backup of ${db}; bad copy removed"
    fi

    gzip -f "${out}"
    backed_up=$((backed_up + 1))
done

if [[ "${backed_up}" -eq 0 ]]; then
    note "WARNING: no databases found in ${DATA_DIR} — 0 files backed up."
    note "         Expected right after first install (the orchestrator creates the"
    note "         DBs on first use). Investigate if it persists past the first day."
    exit 0
fi

# Prune. find's -mtime +N matches files modified more than N days ago
# (i.e. archive's mtime is the stamp at write time, so this is "older
# than N days from now"). Files newly written today are mtime 0 and
# never match +N for N >= 1.
note "pruning archives older than ${RETENTION_DAYS} days from ${BACKUP_DIR}"
find "${BACKUP_DIR}" -maxdepth 1 -type f -name '*.db.gz' -mtime +"${RETENTION_DAYS}" -print -delete

note "done — ${backed_up} db(s) backed up; retention ${RETENTION_DAYS}d"
