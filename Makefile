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

# All supported cross-compile targets (M7): the codebase is pure Go
# (CGO_ENABLED=0, no cgo), so every verified Go os/arch combination is
# supported; ARM is offered as v6 (32-bit Pi Zero/1) and v7 (Pi 3/4).
# Format: "GOOS GOARCH NAME GOARM" (GOARM only when GOARCH=arm).

.PHONY: build-all
build-all: ## Cross-build every supported target (frame + updater)
	@for t in \
		"linux amd64 amd64" "linux arm64 arm64" "linux 386 386" \
		"linux riscv64 riscv64" "linux arm armv6 6" "linux arm armv7 7" \
		"darwin amd64 amd64" "darwin arm64 arm64" \
		"windows amd64 amd64" "windows arm64 arm64"; do \
		set -- $$t; os=$$1; arch=$$2; name=$$3; goarm=$$4; \
		for c in $(CMDS); do \
			out=$(DIST)/openframe-$$os-$$name/$$c; \
			[ "$$os" = "windows" ] && out="$$out.exe"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOARM=$$goarm $(GO) build -ldflags "$(LDFLAGS)" -o "$$out" ./cmd/$$c || exit 1; \
		done; \
	done
	$(MAKE) checksums

.PHONY: checksums
checksums: ## SHA-256 checksums for dist artifacts (all targets)
	cd $(DIST) && find . -type f ! -name SHA256SUMS.txt -exec sha256sum {} \; | sort -k2 > SHA256SUMS.txt

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

.PHONY: bench
bench: ## Performance benchmarks (photos pipeline + manifest build)
	$(GO) test ./internal/server/photos/ ./internal/server/manifest/ -run xxx -bench . -benchmem

.PHONY: soak
soak: ## Long-duration reliability: repeated race suites + sustained mixed load
	$(GO) test ./... -race -count=3
	OPENFRAME_SOAK_SECONDS=$(SOAK_SECONDS) $(GO) test ./internal/server/api/ -run TestSoakMixedLoad -race -count=1 -timeout 2h

export SOAK_SECONDS ?= 60

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
