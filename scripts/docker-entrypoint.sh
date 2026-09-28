#!/bin/bash
# Supervise the muxer stack + daemon: if any dies, tear the rest down so
# Docker's restart policy brings them back together.
set -euo pipefail

MUX_LOG_LEVEL="${AIRVAULT_MUX_LOG_LEVEL:-warn}"

export AIRVAULT_CONFIG_DIR="${AIRVAULT_CONFIG_DIR:-/config}"
export AIRVAULT_LOCKDOWN_DIR="${AIRVAULT_LOCKDOWN_DIR:-$AIRVAULT_CONFIG_DIR/lockdown}"
export AIRVAULT_BACKUP_DIR="${AIRVAULT_BACKUP_DIR:-/backups}"

LOCKDOWN="$AIRVAULT_LOCKDOWN_DIR"
install -d -m 0700 "$LOCKDOWN"

# With PUID set, the daemon and netmuxd run as PUID:PGID so written files are
# share-friendly; usbmuxd stays root for raw USB. Unset PUID = everything root.
# /backups is chowned non-recursively on purpose: it can hold terabytes.
PUID="${PUID:-}"
PGID="${PGID:-$PUID}"
if [[ -n "${UMASK:-}" ]]; then umask "$UMASK"; fi

RUN_AS=()
SOCKET_DIR=/var/run/airvault
install -d "$SOCKET_DIR"
if [[ -n "$PUID" ]]; then
  RUN_AS=(setpriv --reuid "$PUID" --regid "$PGID" --clear-groups)
  chown "$PUID:$PGID" "$SOCKET_DIR" "$AIRVAULT_BACKUP_DIR"
  chown -R "$PUID:$PGID" "$AIRVAULT_CONFIG_DIR" "$LOCKDOWN"
fi

# usbmuxd reads its record store at /var/lib/lockdown — point it at the shared
# lockdown dir wherever this install keeps it.
[ -L /var/lib/lockdown ] || rm -rf /var/lib/lockdown
ln -sfn "$LOCKDOWN" /var/lib/lockdown

# The muxers are separate processes and cannot use AirVault's slog renderer.
# Prefix each complete line so Docker/Unraid output still has an unambiguous
# source. exec keeps the captured PID equal to the actual daemon PID.
prefix_lines() {
  local component="$1" line
  while IFS= read -r line || [[ -n "$line" ]]; do
    printf '[%s] %s\n' "$component" "$line"
  done
}

run_component() {
  local component="$1"
  shift
  exec "$@" > >(prefix_lines "$component") 2>&1
}

# libusb usbmuxd owns USB at /var/run/usbmuxd. -p disables its preflight so
# only AirVault's pair wizard ever triggers the Trust dialog.
USBMUXD_ARGS=(-f -p)
if [[ "$MUX_LOG_LEVEL" == "debug" || "$MUX_LOG_LEVEL" == "trace" ]]; then
  USBMUXD_ARGS+=(-v)
fi
run_component usbmuxd usbmuxd "${USBMUXD_ARGS[@]}" &
USBMUXD=$!
for _ in $(seq 1 100); do [ -S /var/run/usbmuxd ] && break; sleep 0.1; done
[ -S /var/run/usbmuxd ] || echo "[entrypoint] usbmuxd socket absent after 10s" >&2
chmod 0666 /var/run/usbmuxd 2>/dev/null || true

# netmuxd serves usbmuxd's USB devices plus its own Wi-Fi discoveries; the
# upstream disables its own USB backend. It owns the single iOS heartbeat per
# device (AirVault runs none), so never pass --disable-heartbeat.
run_component netmuxd "${RUN_AS[@]}" env RUST_LOG="$MUX_LOG_LEVEL" netmuxd \
        --upstream-usbmuxd /var/run/usbmuxd \
        --socket-path "$SOCKET_DIR/usbmuxd-net" \
        --plist-storage "$LOCKDOWN" &
NETMUXD=$!
for _ in $(seq 1 100); do [ -S "$SOCKET_DIR/usbmuxd-net" ] && break; sleep 0.1; done
[ -S "$SOCKET_DIR/usbmuxd-net" ] || echo "[entrypoint] netmuxd socket absent after 10s" >&2

# The engine connects to netmuxd (no colon -> unix path).
export USBMUXD_SOCKET_ADDRESS="$SOCKET_DIR/usbmuxd-net"
"${RUN_AS[@]}" airvault &
APP=$!

term() { kill -TERM "$APP" "$NETMUXD" "$USBMUXD" 2>/dev/null || true; }
trap term TERM INT

wait -n "$APP" "$NETMUXD" "$USBMUXD"; rc=$?
term
wait || true
exit "$rc"
