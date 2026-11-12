# ADR-0001: Single Go repository, unified CLI

**Status:** accepted (milestone 1)

## Context

OpenFrame has three long-lived processes (management server, frame
application, updater) that share protocol definitions, domain rules and
configuration semantics. Splitting them across repositories would
fragment protocol versioning and make coordinated releases awkward.

## Decision

One repository, one Go module (`github.com/matthewgall/openframe`), one
CLI binary `openframe` with sub-commands (`server`, `frame`, `pair`,
`doctor`, `status`, `version`), plus a separate `openframe-updater`
binary from the same tree (M6). Go ≥ 1.26, standard library first.

Package boundaries keep the three processes decoupled: `internal/server`
never imports display code; `internal/frame` never imports server
storage backends; `internal/updater` (M6) depends on neither.

## Consequences

- Protocol changes are one commit away on both sides; integration tests
  can spin up both processes in one CI job.
- The updater is a separate binary (not a sub-command) because it runs
  with different privileges and must outlive/replace the frame binary.
- The module path `github.com/matthewgall/openframe` mirrors the
  upstream remote (renamed from a placeholder in M1).
