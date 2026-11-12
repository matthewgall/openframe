# ADR-0002: Pure-Go SQLite driver (modernc.org/sqlite)

**Status:** accepted (milestone 1)

## Context

Both server and frame use SQLite for persistence. `mattn/go-sqlite3` is
the traditional choice but is cgo: building for Raspberry Pi (linux/
arm64) from an amd64 dev box or CI requires a cross C toolchain, and
cgo binaries are less portable (glibc sensitivity, hardlinked
libraries).

## Decision

Use `modernc.org/sqlite` (pure Go, registered as driver `"sqlite"`).
WAL mode, `busy_timeout`, `foreign_keys=ON` and `_txlock=immediate` are
set per-connection via the DSN; the pool is bounded (max 4 connections)
to keep memory predictable on 1 GB hardware.

Version pinning: the module targets Go ≥ 1.26 (spec §3), so
`golang.org/x/*` dependencies follow the current release line
(`x/crypto` v0.59.x, `x/image` v0.48.x, `x/sys` v0.50.x — all
`go 1.26.0`). `modernc.org/sqlite` v1.59.x and `modernc.org/libc`
v1.75.x remain the SQLite pair. CI pins the exact toolchain via
`setup-go` (matching `go.mod`), so builds are reproducible regardless
of the runner's default Go.

## Consequences

- `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build` works from any host;
  CI cross-builds stay trivial.
- Performance is adequate for this workload (metadata DBs, small cache
  indexes); the hot path in M3 (file download + hash) never goes
  through SQLite. Revisit if benchmarks in M7 say otherwise.
- Pure-Go driver means larger binary (~30 MB) — acceptable for a
  long-lived appliance.
