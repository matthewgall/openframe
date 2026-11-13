# OpenFrame build & development entry points.

GO      ?= go
NODE    ?= node
NPM     ?= npm
BIN     := bin
DIST    := dist

VERSION := 0.1.0-dev
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/matthewgall/openframe/internal/version.Version=$(VERSION) \
           -X github.com/matthewgall/openframe/internal/version.Commit=$(COMMIT) \
           -X github.com/matthewgall/openframe/internal/version.Date=$(DATE)

GOOS_LINUX := linux

.PHONY: all
all: fmt-check vet test build

## --- Build ------------------------------------------------------------

# Both binaries ship in every release (frame + privileged updater, M6).
CMDS := openframe openframe-updater

.PHONY: build
build: ## Build openframe for the host platform
	@for c in $(CMDS); do \
		CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/$$c ./cmd/$$c; \
	done

.PHONY: build-all
build-all: ## Cross-build linux/amd64 and linux/arm64 (frame + updater)
	@for c in $(CMDS); do \
		CGO_ENABLED=0 GOOS=$(GOOS_LINUX) GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o $(DIST)/openframe-linux-amd64/$$c ./cmd/$$c; \
		CGO_ENABLED=0 GOOS=$(GOOS_LINUX) GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o $(DIST)/openframe-linux-arm64/$$c ./cmd/$$c; \
	done
	$(MAKE) checksums

.PHONY: checksums
checksums: ## SHA-256 checksums for dist artifacts (frame + updater)
	cd $(DIST) && sha256sum openframe-linux-*/openframe openframe-linux-*/openframe-updater > SHA256SUMS.txt

## --- Web admin UI -------------------------------------------------------

.PHONY: web-install
web-install: ## Install web dependencies
	cd web/admin && $(NPM) install

.PHONY: web
web: ## Build the admin SPA into web/admin/dist (committed; embedded by Go)
	cd web/admin && $(NPM) run build

.PHONY: web-dev
web-dev: ## Run the SPA dev server (proxies /healthz and /api to :8080)
	cd web/admin && $(NPM) run dev

## --- Quality ------------------------------------------------------------

.PHONY: fmt
fmt: ## gofmt all sources
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any file is unformatted
	@out=$$($(GO) fmt ./... 2>&1); if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: test
test: ## Unit + integration tests
	$(GO) test ./... -count=1

.PHONY: test-race
test-race: ## Tests with the race detector
	$(GO) test ./... -race -count=1

.PHONY: vuln
vuln: ## Dependency vulnerability scan
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

## --- Run (development) ----------------------------------------------------

.PHONY: run-server
run-server: ## Run the server locally (state in ./.data-dev)
	OPENFRAME_DATA_DIR=$$PWD/.data-dev $(BIN)/openframe server --log-level debug

.PHONY: run-frame
run-frame: ## Run the headless frame locally
	OPENFRAME_DATA_DIR=$$PWD/.data-dev $(BIN)/openframe frame --log-level debug

.PHONY: doctor
doctor:
	$(BIN)/openframe doctor

.PHONY: status
status:
	$(BIN)/openframe status

## --- Housekeeping ---------------------------------------------------------

.PHONY: clean
clean:
	rm -rf $(BIN) $(DIST) .data-dev

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'
