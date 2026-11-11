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

Version pinning: the module targets Go 1.25, so dependencies that
require a newer Go are pinned to the latest compatible release
(`modernc.org/sqlite` v1.59.x, `golang.org/x/sys` v0.47.x,
`modernc.org/libc` v1.75.x) with `GOTOOLCHAIN=local` enforced in CI.

## Consequences

- `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build` works from any host;
  CI cross-builds stay trivial.
- Performance is adequate for this workload (metadata DBs, small cache
  indexes); the hot path in M3 (file download + hash) never goes
  through SQLite. Revisit if benchmarks in M7 say otherwise.
- Pure-Go driver means larger binary (~30 MB) — acceptable for a
  long-lived appliance.
