#!/usr/bin/env bash
# Deploy tcp-commander to a remote Linux host that runs it as a systemd
# service. Assumes scripts/install-systemd.sh has already been run there.
#
# Steps:
#   1. Cross-compile the daemon for linux/amd64 via `make linux-amd64`.
#   2. scp the new binary + config as staging files (`*.new`).
#   3. In one SSH transaction: back up the live binary/config, atomically
#      swap in the new ones, `systemctl restart`, and verify the service
#      is active, listening, and answering `ping`.
#
# Defaults assume your layout: ssh-key login to root@10.10.1.30 with
# REMOTE_DIR=/root/tcp-commander and the systemd unit named tcpcommanderd.
# Override with env vars:
#
#   HOST=10.10.1.30
#   REMOTE_USER=root
#   REMOTE_DIR=/root/tcp-commander
#   SSH_PORT=22
#   CONFIG_SRC=examples/config.yaml
#   SERVICE_NAME=tcpcommanderd
#   LISTEN_PORT=9000           must match `listen:` in config.yaml
#   HEALTHCHECK_TIMEOUT=15     seconds to wait for ping to return rc=0
#   SKIP_RESTART=1             upload only — leave live files untouched
#
# Usage:
#   scripts/deploy.sh                  # build, upload, restart, verify
#   SKIP_RESTART=1 scripts/deploy.sh   # stage for a manual swap later

set -euo pipefail

HOST="${HOST:-10.10.1.30}"
REMOTE_USER="${REMOTE_USER:-root}"
REMOTE_DIR="${REMOTE_DIR:-/root/tcp-commander}"
SSH_PORT="${SSH_PORT:-22}"
CONFIG_SRC="${CONFIG_SRC:-examples/config.yaml}"
SERVICE_NAME="${SERVICE_NAME:-tcpcommanderd}"
LISTEN_PORT="${LISTEN_PORT:-9000}"
HEALTHCHECK_TIMEOUT="${HEALTHCHECK_TIMEOUT:-15}"
SKIP_RESTART="${SKIP_RESTART:-0}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

LOCAL_BIN="dist/linux-amd64/tcpcommanderd"
REMOTE="${REMOTE_USER}@${HOST}"
SSH=(ssh -p "${SSH_PORT}" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10)
SCP=(scp -P "${SSH_PORT}" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10)

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

[[ -f "$CONFIG_SRC" ]] || die "config not found: $CONFIG_SRC"

log "Building linux/amd64 binary"
make linux-amd64 >/dev/null
[[ -x "$LOCAL_BIN" ]] || die "build did not produce $LOCAL_BIN"
log "Built $(du -h "$LOCAL_BIN" | awk '{print $1}') -> $LOCAL_BIN"

log "Checking systemd unit on ${REMOTE}"
"${SSH[@]}" "$REMOTE" "test -d '${REMOTE_DIR}'" \
  || die "remote dir ${REMOTE_DIR} missing — create it or run scripts/install-systemd.sh"
"${SSH[@]}" "$REMOTE" "systemctl list-unit-files | grep -q '^${SERVICE_NAME}\\.service'" \
  || die "${SERVICE_NAME}.service not installed on ${HOST} — run scripts/install-systemd.sh first"

log "Uploading binary -> ${REMOTE}:${REMOTE_DIR}/tcpcommanderd.new"
"${SCP[@]}" "$LOCAL_BIN" "${REMOTE}:${REMOTE_DIR}/tcpcommanderd.new"
"${SSH[@]}" "$REMOTE" "chmod +x '${REMOTE_DIR}/tcpcommanderd.new'"

log "Uploading config (${CONFIG_SRC}) -> ${REMOTE}:${REMOTE_DIR}/config.yaml.new"
"${SCP[@]}" "$CONFIG_SRC" "${REMOTE}:${REMOTE_DIR}/config.yaml.new"

if [[ "$SKIP_RESTART" = "1" ]]; then
  log "SKIP_RESTART=1: staging files in place, leaving live service untouched."
  log "  Live swap can be done manually:"
  cat <<EOF
    ssh ${REMOTE}
    cd ${REMOTE_DIR}
    mv tcpcommanderd.new tcpcommanderd && mv config.yaml.new config.yaml
    systemctl restart ${SERVICE_NAME}
EOF
  exit 0
fi

TS="$(date -u +%Y%m%dT%H%M%SZ)"
log "Swapping + restarting ${SERVICE_NAME} on ${REMOTE} (TS=${TS})"

"${SSH[@]}" "$REMOTE" \
  REMOTE_DIR="$REMOTE_DIR" \
  SERVICE_NAME="$SERVICE_NAME" \
  LISTEN_PORT="$LISTEN_PORT" \
  HEALTHCHECK_TIMEOUT="$HEALTHCHECK_TIMEOUT" \
  TS="$TS" \
  'bash -s' <<'REMOTE_EOF'
set -euo pipefail
: "${REMOTE_DIR:?}" "${SERVICE_NAME:?}" "${LISTEN_PORT:?}" "${HEALTHCHECK_TIMEOUT:?}" "${TS:?}"

cd "${REMOTE_DIR}"
[[ -x ./tcpcommanderd.new ]] || { echo "missing tcpcommanderd.new"; exit 1; }
[[ -f ./config.yaml.new   ]] || { echo "missing config.yaml.new";   exit 1; }

# Back up live files for easy rollback.
[[ -f ./tcpcommanderd ]] && cp -p ./tcpcommanderd "./tcpcommanderd.bak.${TS}"
[[ -f ./config.yaml   ]] && cp -p ./config.yaml   "./config.yaml.bak.${TS}"

# Atomically swap in the new files. systemd Type=simple re-execs on restart.
mv -f ./tcpcommanderd.new ./tcpcommanderd
mv -f ./config.yaml.new   ./config.yaml

# Ensure parent dir for any logging.file path exists. Best-effort grep —
# YAML parsing in awk isn't perfect, but this matches the canonical
# `  file: /path/to.log` form used in examples/config.yaml.
LOGFILE=$(awk '
  /^logging:[[:space:]]*$/ { in_log=1; next }
  /^[^[:space:]]/          { in_log=0 }
  in_log && /^[[:space:]]+file:[[:space:]]*/ {
    sub(/^[[:space:]]+file:[[:space:]]*/, "")
    sub(/[[:space:]]*#.*$/, "")
    gsub(/^"|"$|^'\''|'\''$/, "")
    print; exit
  }' ./config.yaml 2>/dev/null || true)
if [[ -n "${LOGFILE:-}" ]]; then
  install -d -m 0755 "$(dirname "${LOGFILE}")"
fi

systemctl restart "${SERVICE_NAME}"

echo "==> Waiting up to ${HEALTHCHECK_TIMEOUT}s for service to become healthy"
for i in $(seq 1 "${HEALTHCHECK_TIMEOUT}"); do
  if ! systemctl is-active --quiet "${SERVICE_NAME}"; then
    sleep 1; continue
  fi
  if ! ss -ltn | awk '{print $4}' | grep -qE "[:.]${LISTEN_PORT}\$"; then
    sleep 1; continue
  fi
  if command -v nc >/dev/null 2>&1; then
    RESP=$(printf '{"id":"hc","cmd":"ping"}\n' | nc -w 2 127.0.0.1 "${LISTEN_PORT}" 2>/dev/null || true)
    if echo "${RESP}" | grep -q '"rc":0'; then
      echo "healthcheck OK: ${RESP}"
      systemctl --no-pager --lines=3 status "${SERVICE_NAME}" || true
      exit 0
    fi
  else
    echo "service active and listening (install netcat-openbsd to enable ping check)"
    exit 0
  fi
  sleep 1
done

echo "ERROR: ${SERVICE_NAME} did not become healthy in ${HEALTHCHECK_TIMEOUT}s" >&2
systemctl --no-pager --lines=20 status "${SERVICE_NAME}" || true
echo "--- recent journal ---"
journalctl -u "${SERVICE_NAME}" --no-pager --lines=20 || true
echo "--- rollback hint ---"
echo "  cd ${REMOTE_DIR} && mv tcpcommanderd.bak.${TS} tcpcommanderd && mv config.yaml.bak.${TS} config.yaml && systemctl restart ${SERVICE_NAME}"
exit 1
REMOTE_EOF

log "Deploy complete."
