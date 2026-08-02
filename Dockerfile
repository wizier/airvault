# netmuxd ships a prebuilt binary; the digest pins this exact tag. The image is
# built for linux/amd64 only, so no other release is fetched.
ARG NETMUXD_TAG=v0.4.3
ARG NETMUXD_SHA256=85b6598284fc639f2a282584461d05e2090b79bdf3ec949d2a5e5d3dc655dde4

# ── 1. Svelte SPA -> web/dist ──
FROM node:22-slim AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./

# ARG after npm ci: a version bump must not invalidate the install layer.
ARG VERSION=dev
RUN VITE_APP_VERSION=$VERSION npm run build

# ── 2. Rust idevice shim -> static lib for cgo ──
FROM rust:1.97.1-trixie AS shim
RUN apt-get update && apt-get install -y --no-install-recommends libssl-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY engine/rust/ ./
RUN cargo build --release --locked \
 && mkdir -p /out && cp target/release/libairvault_shim.a /out/

# ── 3. Go daemon (cgo, links the shim) ──
FROM golang:1.26-trixie AS build
# .git is dockerignored; the release version comes in as an ARG.
ARG VERSION=dev
RUN apt-get update && apt-get install -y --no-install-recommends libssl-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=web /web/dist ./web/dist
COPY --from=shim /out/libairvault_shim.a ./engine/rust/target/link/libairvault_shim.a

RUN make build COMPONENTS= VERSION=${VERSION} GOTAGS=timetzdata DAEMON_BINARY=/out/airvault
RUN strip /out/airvault

# ── 4. Runtime ──
FROM debian:trixie-slim AS runtime
ARG VERSION=dev
LABEL org.opencontainers.image.title="AirVault" \
      org.opencontainers.image.description="Self-hosted iPhone backup server " \
      org.opencontainers.image.source="https://github.com/wizier/airvault" \
      org.opencontainers.image.licenses="GPL-3.0" \
      org.opencontainers.image.version="$VERSION"

# usbmuxd: USB transport; netmuxd: Wi-Fi on top; util-linux: setpriv for PUID/PGID
RUN apt-get update && apt-get install -y --no-install-recommends usbmuxd ca-certificates libssl3t64 curl util-linux \
    && rm -rf /var/lib/apt/lists/*

ARG NETMUXD_TAG
ARG NETMUXD_SHA256
RUN set -eux; \
    curl -fsSL -o /tmp/netmuxd.tar.gz \
      "https://github.com/jkcoxson/netmuxd/releases/download/$NETMUXD_TAG/netmuxd-x86_64-unknown-linux-gnu.tar.gz"; \
    echo "$NETMUXD_SHA256  /tmp/netmuxd.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/netmuxd.tar.gz -C /usr/local/bin netmuxd; \
    rm /tmp/netmuxd.tar.gz
COPY --from=build /out/airvault /usr/local/bin/airvault
COPY --chmod=0755 scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY LICENSE /licenses/

# Storage: /config (state, pairing records) + /backups.
EXPOSE 8080
ENV AIRVAULT_BIND_HOST=0.0.0.0

# Health dot for NAS UIs (unRAID / Synology / TrueNAS).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS "http://127.0.0.1:${AIRVAULT_PORT:-8080}/healthz" || exit 1
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
