#!/usr/bin/env bash
# Install tcp-commander as a systemd service on a remote Linux host.
#
# Assumes:
#   - The binary already lives at ${REMOTE_DIR}/tcpcommanderd
#     (run scripts/deploy.sh first if not).
#   - The config already lives at ${REMOTE_DIR}/config.yaml.
#   - The daemon should run as root from ${REMOTE_DIR}. This matches
#     the personal-use layout under /root/tcp-commander; if you want
#     a hardened cmdrunner-user setup, use examples/tcpcommanderd.service
#     instead.
#
# What it does, in one SSH transaction (so the listen port is never both
# occupied by the manual instance and re-bound by systemd):
#   1. SIGTERMs any manually-launched tcpcommanderd, escalates to SIGKILL.
#   2. Verifies port 9000 is free.
#   3. Writes /etc/systemd/system/tcpcommanderd.service.
#   4. systemctl daemon-reload && enable --now.
#   5. Prints status, port binding, and the ping result so you can
#      confirm it's actually serving.
#
# Defaults:
#   HOST=10.10.1.30
#   REMOTE_USER=root
#   REMOTE_DIR=/root/tcp-commander
#   SSH_PORT=22
#   LISTEN_PORT=9000   (must match `listen:` in config.yaml)

set -euo pipefail

HOST="${HOST:-10.10.1.30}"
REMOTE_USER="${REMOTE_USER:-root}"
REMOTE_DIR="${REMOTE_DIR:-/root/tcp-commander}"
SSH_PORT="${SSH_PORT:-22}"
LISTEN_PORT="${LISTEN_PORT:-9000}"

REMOTE="${REMOTE_USER}@${HOST}"
SSH=(ssh -p "${SSH_PORT}" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10)

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

log "Installing systemd unit on ${REMOTE} (REMOTE_DIR=${REMOTE_DIR})"

"${SSH[@]}" "$REMOTE" \
  REMOTE_DIR="$REMOTE_DIR" LISTEN_PORT="$LISTEN_PORT" \
  'bash -s' <<'REMOTE_EOF'
set -euo pipefail

: "${REMOTE_DIR:?}"
: "${LISTEN_PORT:?}"

[[ -x "${REMOTE_DIR}/tcpcommanderd" ]] || { echo "binary missing: ${REMOTE_DIR}/tcpcommanderd"; exit 1; }
[[ -f "${REMOTE_DIR}/config.yaml"   ]] || { echo "config missing: ${REMOTE_DIR}/config.yaml";   exit 1; }

echo "==> Stopping any manually-launched instance"
pkill -TERM -x tcpcommanderd 2>/dev/null || true
for _ in 1 2 3 4 5 6 7 8 9 10; do
  pgrep -x tcpcommanderd >/dev/null || break
  sleep 0.3
done
pkill -KILL -x tcpcommanderd 2>/dev/null || true
sleep 0.3

if ss -ltn | awk '{print $4}' | grep -qE "[:.]${LISTEN_PORT}\$"; then
  echo "port ${LISTEN_PORT} still bound, aborting:"
  ss -ltnp | grep ":${LISTEN_PORT} " || true
  exit 1
fi
echo "port ${LISTEN_PORT} is free"

echo "==> Writing /etc/systemd/system/tcpcommanderd.service"
cat >/etc/systemd/system/tcpcommanderd.service <<UNIT
[Unit]
Description=tcp-commander remote command execution daemon
Documentation=https://github.com/yuanmomo/tcp-commander
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
WorkingDirectory=${REMOTE_DIR}
ExecStart=${REMOTE_DIR}/tcpcommanderd --config ${REMOTE_DIR}/config.yaml

# Graceful shutdown: matches the daemon's SIGTERM handling. Allow a margin
# above the daemon's own kill_grace (default 10s) for in-flight commands.
KillSignal=SIGTERM
TimeoutStopSec=35

# Daemon writes JSON logs to stdout; journald captures them.
StandardOutput=journal
StandardError=journal

Restart=on-failure
RestartSec=2s

[Install]
WantedBy=multi-user.target
UNIT

echo "==> systemctl daemon-reload && enable --now"
systemctl daemon-reload
systemctl enable --now tcpcommanderd

echo "==> Status"
systemctl --no-pager --lines=10 status tcpcommanderd || true

echo "==> Port"
ss -ltn | grep ":${LISTEN_PORT} " || echo "(no listener on ${LISTEN_PORT})"

echo "==> Self-test (ping via loopback)"
if command -v nc >/dev/null 2>&1; then
  printf '{"id":"hc","cmd":"ping"}\n' | nc -w 3 127.0.0.1 "${LISTEN_PORT}" || echo "(ping failed)"
else
  echo "(install netcat-openbsd to enable ping self-test)"
fi
REMOTE_EOF
