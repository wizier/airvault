GO              ?= go
CARGO           ?= cargo
NPM             ?= npm
DOCKER          ?= docker
AIR             ?= $(GO) tool air
GOLANGCI_LINT   ?= golangci-lint
CBINDGEN        ?= cbindgen
CBINDGEN_VERSION := 0.29.4
VERSION         ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE           ?= ghcr.io/wizier/airvault
PLATFORM        ?= linux/amd64
DAEMON_BINARY   ?= bin/airvault
GOTAGS          ?=
COMPONENTS      ?= shim web

ifeq ($(shell uname -s),Darwin)
OPENSSL_STATIC ?= 1
export OPENSSL_STATIC
endif

RUST_DIR    := engine/rust
SHIM_REL    := $(RUST_DIR)/target/release/libairvault_shim.a
SHIM_DBG    := $(RUST_DIR)/target/debug/libairvault_shim.a
SHIM_LINK   := $(RUST_DIR)/target/link/libairvault_shim.a
SHIM_HEADER := $(RUST_DIR)/include/airvault.h
RUST_SRC    := $(shell find $(RUST_DIR)/src -name '*.rs' 2>/dev/null) \
	$(RUST_DIR)/Cargo.toml $(RUST_DIR)/Cargo.lock rust-toolchain.toml
WEB_DEPS    := web/node_modules/.package-lock.json
GO_LDFLAGS  := -s -w -buildid= \
	-X github.com/wizier/airvault/internal/version.AppVersion=$(VERSION)
GO_BUILD    = CGO_ENABLED=1 $(GO) build $(if $(GOTAGS),-tags=$(GOTAGS)) \
	-trimpath -ldflags="$(GO_LDFLAGS)"

.PHONY: build docker-build web shim shim-dev ffi-header ffi-header-check \
	cbindgen-version dev dev-air dev-vite fmt lint test check clean help

build: $(COMPONENTS) ## Build the production binary
	$(GO_BUILD) -o $(DAEMON_BINARY) ./cmd/airvault

docker-build: ## Build the Docker image
	$(DOCKER) build --platform "$(PLATFORM)" --build-arg VERSION="$(VERSION)" \
		--tag "$(IMAGE):$(VERSION)" .

$(WEB_DEPS): web/package.json web/package-lock.json
	cd web && $(NPM) ci

web: $(WEB_DEPS) ## Build the web application
	cd web && VITE_APP_VERSION=$(VERSION) $(NPM) run build

cbindgen-version:
	@test "$$($(CBINDGEN) --version)" = "cbindgen $(CBINDGEN_VERSION)" || { echo "install cbindgen $(CBINDGEN_VERSION)"; exit 1; }

ffi-header: cbindgen-version ## Regenerate the FFI header
	$(CBINDGEN) --quiet --only-target-dependencies --config engine/rust/cbindgen.toml \
		--crate airvault_shim --output $(SHIM_HEADER) engine/rust

ffi-header-check: cbindgen-version
	$(CBINDGEN) --quiet --only-target-dependencies --verify --config engine/rust/cbindgen.toml \
		--crate airvault_shim --output $(SHIM_HEADER) engine/rust

shim: ## Build the Rust shim
	cd $(RUST_DIR) && $(CARGO) build --release --locked
	@mkdir -p $(dir $(SHIM_LINK)) && cp $(SHIM_REL) $(SHIM_LINK)

# go:embed requires a file when only the backend is being checked.
web/dist/index.html:
	@mkdir -p web/dist
	@printf '<!doctype html><title>AirVault</title>UI not built - run make web' > $@

$(SHIM_LINK): $(RUST_SRC)
	cd $(RUST_DIR) && $(CARGO) build --locked
	@mkdir -p $(dir $(SHIM_LINK)) && cp $(SHIM_DBG) $(SHIM_LINK)

shim-dev: $(SHIM_LINK)

dev: $(WEB_DEPS) web/dist/index.html ## Run the development servers
	@$(MAKE) --no-print-directory -j2 dev-air dev-vite

dev-air:
	$(AIR)

dev-vite:
	cd web && $(NPM) run dev

fmt: ## Format source code
	$(GO) fmt ./...
	$(CARGO) fmt --manifest-path $(RUST_DIR)/Cargo.toml --all

lint: shim-dev $(WEB_DEPS) web/dist/index.html ## Run static analysis
	CGO_ENABLED=1 $(GOLANGCI_LINT) run ./...
	$(CARGO) fmt --manifest-path $(RUST_DIR)/Cargo.toml --all -- --check
	$(CARGO) clippy --manifest-path $(RUST_DIR)/Cargo.toml --locked --all-targets -- -D warnings
	cd web && $(NPM) run check

test: shim-dev web/dist/index.html ## Run tests
	CGO_ENABLED=1 $(GO) test ./...
	$(CARGO) test --manifest-path $(RUST_DIR)/Cargo.toml --locked --lib

check: ffi-header-check lint test ## Run all checks

clean: ## Remove build artifacts
	rm -rf bin/ .air/ web/dist
	$(CARGO) clean --manifest-path $(RUST_DIR)/Cargo.toml

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*?## "}; /^[a-zA-Z0-9_-]+:.*?## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
