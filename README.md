# AirVault - *Self-hosted, wireless iPhone backups*

## Quick start

```bash
docker compose up -d
```

## Volumes

| Container path | Holds | Put it on |
|---|---|---|
| `/config` | settings, database, pairing records | app-data storage |
| `/backups` | the backups | a share with room to grow |

Nothing else needs to persist.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `AIRVAULT_PORT` | `8080` | Web UI port |
| `AIRVAULT_AUTH_TOKEN` | *(generated)* | Web UI password; when unset, the stored one is printed in the log |
| `AIRVAULT_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `TZ` | `Etc/UTC` | Timezone for log timestamps and downloaded backup names |
| `PUID` / `PGID` | *(unset — run as root)* | Backups and settings are written as this uid:gid (`99`/`100` on Unraid; `id` shows yours) |
| `UMASK` | *(unset)* | Permission mask for files the container creates |

Paths can be overridden with `AIRVAULT_CONFIG_DIR`, `AIRVAULT_BACKUP_DIR` and
`AIRVAULT_LOCKDOWN_DIR`.

Turning `PUID` on for an install that already ran as root needs a one-time
`chown -R <uid>:<gid>` of the backups folder; new files are owned correctly
from then on.

## Building from source

```bash
make build          # production binary
make test           # Go and Rust tests
make lint           # Go, Rust and Svelte static analysis
make check          # complete local/CI gate
make dev            # live-reload daemon and UI
make docker-build   # local container image
```

Builds need Go, Rust (pinned in `rust-toolchain.toml`) and Node. The complete
check also needs `golangci-lint` and cbindgen 0.29.4.

## License

GNU General Public License v3.0 or later — see [LICENSE](LICENSE).
