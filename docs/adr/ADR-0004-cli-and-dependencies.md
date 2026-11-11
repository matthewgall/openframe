# ADR-0004: Hand-rolled CLI, minimal dependency set

**Status:** accepted (milestone 1)

## Context

OpenFrame has six flat sub-commands. Frameworks (cobra, urfave/cli) add
a dependency tree and behavioural surface (persistence, suggestions,
nested commands) none of which are needed, while the spec explicitly
favours the standard library and minimal dependencies.

## Decision

The CLI is a hand-rolled dispatcher over `flag.FlagSet`
(`internal/cli/cli.go`): a flat `[]command{name, short, run}` table,
global-ish flags registered per command (`--config`, `--log-level`,
`--json`), and explicit exit codes (`0` ok, `1` error/fail, `2` usage).

Dependency budget for the shared + server packages (M1):
`modernc.org/sqlite`, `gopkg.in/yaml.v3`, `github.com/go-chi/chi/v5`
(plus their transitive deps). Each new dependency needs a justification
in its PR/commit message.

## Consequences

- No framework surprises; the whole CLI is ~600 lines and fully testable
  by calling `Execute` with captured stdout.
- `govulncheck` + dependabot keep the small tree honest.
- If sub-command count grows past ~15 or nested grouping becomes
  wanted, revisit a framework — the dispatcher is isolated in one
  package so the switch is contained.
