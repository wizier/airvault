#!/bin/bash
# Supervise the muxer stack + daemon: if any dies, tear the rest down so
# Docker's restart policy brings them back together.
set -euo pipefail

MUX_LOG_LEVEL="${AIRVAULT_MUX_LOG_LEVEL:-warn}"

# Storage layout: /config (app state, lockdown records inside) + /backups.
export AIRVAULT_CONFIG_DIR="${AIRVAULT_CONFIG_DIR:-/config}"
export AIRVAULT_LOCKDOWN_DIR="${AIRVAULT_LOCKDOWN_DIR:-$AIRVAULT_CONFIG_DIR/lockdown}"
export AIRVAULT_BACKUP_DIR="${AIRVAULT_BACKUP_DIR:-/backups}"

LOCKDOWN="$AIRVAULT_LOCKDOWN_DIR"
install -d -m 0700 "$LOCKDOWN"
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

# USB transport: the mature libusb usbmuxd owns /var/run/usbmuxd. -p disables
# its preflight so only AirVault's pair wizard ever triggers the Trust dialog
# (netmuxd's own young nusb USB stack is not used — see --upstream below).
USBMUXD_ARGS=(-f -p)
if [[ "$MUX_LOG_LEVEL" == "debug" || "$MUX_LOG_LEVEL" == "trace" ]]; then
  USBMUXD_ARGS+=(-v)
fi
run_component usbmuxd usbmuxd "${USBMUXD_ARGS[@]}" &
USBMUXD=$!
for _ in $(seq 1 100); do [ -S /var/run/usbmuxd ] && break; sleep 0.1; done
[ -S /var/run/usbmuxd ] || echo "[entrypoint] usbmuxd socket absent after 10s" >&2

# Wi-Fi (mdns-sd) via netmuxd in shim mode over usbmuxd: it serves usbmuxd's USB
# devices plus its own network discoveries on a second socket. Passing an
# upstream muxer disables netmuxd's own USB backend.
#
# netmuxd owns the device's single iOS heartbeat — the Marco/Polo keepalive that
# stops iOS from reaping service connections. iOS allows one heartbeat per device
# and AirVault runs none of its own, so netmuxd keeps it (no --disable-heartbeat).
run_component netmuxd env RUST_LOG="$MUX_LOG_LEVEL" netmuxd \
        --upstream-usbmuxd /var/run/usbmuxd \
        --socket-path /var/run/usbmuxd-net \
        --plist-storage "$LOCKDOWN" &
NETMUXD=$!
for _ in $(seq 1 100); do [ -S /var/run/usbmuxd-net ] && break; sleep 0.1; done
[ -S /var/run/usbmuxd-net ] || echo "[entrypoint] netmuxd socket absent after 10s" >&2

# The shim connects to netmuxd (no colon -> unix path).
export USBMUXD_SOCKET_ADDRESS=/var/run/usbmuxd-net
airvault &
APP=$!

term() { kill -TERM "$APP" "$NETMUXD" "$USBMUXD" 2>/dev/null || true; }
trap term TERM INT

wait -n "$APP" "$NETMUXD" "$USBMUXD"; rc=$?
term
wait || true
exit "$rc"
