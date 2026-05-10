#!/usr/bin/env bash
# Deploy tcp-commander to a remote Linux host.
#
# Steps:
#   1. Cross-compile the daemon for linux/amd64 via `make linux-amd64`.
#   2. scp the binary to ${REMOTE_DIR}/tcpcommanderd on the remote host.
#   3. scp a config to ${REMOTE_DIR}/config.yaml (backs up any existing one).
#
# Defaults assume the layout you described: ssh-key login to root@10.10.1.30
# with ${REMOTE_DIR} = /root/tcp-commander. Override with env vars:
#
#   HOST=10.10.1.30          remote host or alias
#   REMOTE_USER=root         ssh user
#   REMOTE_DIR=/root/tcp-commander
#   SSH_PORT=22
#   CONFIG_SRC=examples/config.yaml   local config to upload
#   RESTART=1                also restart the daemon after deploy (see below)
#
# Optional: if RESTART=1, the script will try to restart `tcpcommanderd`
# via systemd; if no unit is installed, it falls back to killing any
# running process and re-launching it from REMOTE_DIR in the background.
#
# Usage:
#   scripts/deploy.sh                       # build + upload binary + config
#   HOST=prod-1 scripts/deploy.sh           # deploy to a different host
#   RESTART=1 scripts/deploy.sh             # also restart after upload

set -euo pipefail

HOST="${HOST:-10.10.1.30}"
REMOTE_USER="${REMOTE_USER:-root}"
REMOTE_DIR="${REMOTE_DIR:-/root/tcp-commander}"
SSH_PORT="${SSH_PORT:-22}"
CONFIG_SRC="${CONFIG_SRC:-examples/config.yaml}"
RESTART="${RESTART:-0}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

LOCAL_BIN="dist/linux-amd64/tcpcommanderd"
REMOTE="${REMOTE_USER}@${HOST}"
SSH="ssh -p ${SSH_PORT} -o StrictHostKeyChecking=accept-new"
SCP="scp -P ${SSH_PORT} -o StrictHostKeyChecking=accept-new"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

[[ -f "$CONFIG_SRC" ]] || die "config not found: $CONFIG_SRC"

log "Building linux/amd64 binary"
make linux-amd64 >/dev/null
[[ -x "$LOCAL_BIN" ]] || die "build did not produce $LOCAL_BIN"
log "Built $(du -h "$LOCAL_BIN" | awk '{print $1}') -> $LOCAL_BIN"

log "Ensuring ${REMOTE}:${REMOTE_DIR} exists"
$SSH "$REMOTE" "mkdir -p '$REMOTE_DIR'"

log "Uploading binary -> ${REMOTE}:${REMOTE_DIR}/tcpcommanderd"
# Upload to a staging name first, then atomically move into place so we
# never leave a half-written binary that the daemon could exec.
$SCP "$LOCAL_BIN" "${REMOTE}:${REMOTE_DIR}/tcpcommanderd.new"
$SSH "$REMOTE" "chmod +x '${REMOTE_DIR}/tcpcommanderd.new' \
  && mv '${REMOTE_DIR}/tcpcommanderd.new' '${REMOTE_DIR}/tcpcommanderd'"

log "Uploading config (${CONFIG_SRC}) -> ${REMOTE}:${REMOTE_DIR}/config.yaml"
TS="$(date -u +%Y%m%dT%H%M%SZ)"
$SSH "$REMOTE" "if [ -f '${REMOTE_DIR}/config.yaml' ]; then \
  cp -p '${REMOTE_DIR}/config.yaml' '${REMOTE_DIR}/config.yaml.bak.${TS}'; \
  echo 'backed up existing config -> config.yaml.bak.${TS}'; \
fi"
$SCP "$CONFIG_SRC" "${REMOTE}:${REMOTE_DIR}/config.yaml.new"
$SSH "$REMOTE" "mv '${REMOTE_DIR}/config.yaml.new' '${REMOTE_DIR}/config.yaml'"

if [[ "$RESTART" = "1" ]]; then
  log "Restarting daemon on ${REMOTE}"
  $SSH "$REMOTE" "bash -s" <<EOF
set -e
if systemctl list-unit-files 2>/dev/null | grep -q '^tcpcommanderd\\.service'; then
  systemctl restart tcpcommanderd
  systemctl --no-pager --lines=5 status tcpcommanderd || true
else
  pkill -x tcpcommanderd 2>/dev/null || true
  sleep 1
  cd '${REMOTE_DIR}'
  nohup ./tcpcommanderd --config ./config.yaml >> tcpcommanderd.log 2>&1 &
  disown || true
  sleep 1
  pgrep -af tcpcommanderd || { echo "daemon not running"; exit 1; }
fi
EOF
else
  log "Skipping restart (set RESTART=1 to restart automatically)"
fi

log "Verifying remote install"
$SSH "$REMOTE" "ls -lh '${REMOTE_DIR}/tcpcommanderd' '${REMOTE_DIR}/config.yaml'"

log "Done. To run manually:"
cat <<EOF
  ssh ${REMOTE}
  cd ${REMOTE_DIR}
  ./tcpcommanderd --config ./config.yaml
EOF
