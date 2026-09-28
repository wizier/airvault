GO              ?= go
NPM             ?= npm
DOCKER          ?= docker
AIR             ?= $(GO) tool air
GOLANGCI_LINT   ?= golangci-lint
VERSION         ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE           ?= ghcr.io/wizier/airvault
PLATFORM        ?= linux/amd64
DAEMON_BINARY   ?= bin/airvault
GOTAGS          ?=
COMPONENTS      ?= web

export CGO_ENABLED := 0

WEB_DEPS    := web/node_modules/.package-lock.json
GO_LDFLAGS  := -s -w -buildid= \
	-X github.com/wizier/airvault/internal/version.AppVersion=$(VERSION)
# nodynamic: the HEIC decoder runs as WASM only, so the binary stays static.
comma       := ,
GO_TAGS     := $(subst $() ,$(comma),$(strip nodynamic $(GOTAGS)))
GO_BUILD    = $(GO) build -tags=$(GO_TAGS) -trimpath -ldflags="$(GO_LDFLAGS)"

.PHONY: build docker-build web dev dev-air dev-vite fmt lint test check clean help

build: $(COMPONENTS) ## Build the production binary
	$(GO_BUILD) -o $(DAEMON_BINARY) ./cmd/airvault

docker-build: ## Build the Docker image
	$(DOCKER) build --platform "$(PLATFORM)" --build-arg VERSION="$(VERSION)" \
		--tag "$(IMAGE):$(VERSION)" .

$(WEB_DEPS): web/package.json web/package-lock.json
	cd web && $(NPM) ci

web: $(WEB_DEPS) ## Build the web application
	cd web && VITE_APP_VERSION=$(VERSION) $(NPM) run build

# go:embed requires a file when only the backend is being checked.
web/dist/index.html:
	@mkdir -p web/dist
	@printf '<!doctype html><title>AirVault</title>UI not built - run make web' > $@

dev: $(WEB_DEPS) web/dist/index.html ## Run the development servers
	@$(MAKE) --no-print-directory -j2 dev-air dev-vite

dev-air:
	$(AIR)

dev-vite:
	cd web && $(NPM) run dev

fmt: ## Format source code
	$(GO) fmt ./...

lint: $(WEB_DEPS) web/dist/index.html ## Run static analysis
	$(GOLANGCI_LINT) run ./...
	cd web && $(NPM) run check

test: web/dist/index.html ## Run tests
	$(GO) test ./...

check: lint test ## Run all checks

clean: ## Remove build artifacts
	rm -rf bin/ .air/ web/dist

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*?## "}; /^[a-zA-Z0-9_-]+:.*?## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
