# ADR-0003: Configuration — YAML + environment overrides

**Status:** accepted (milestone 1)

## Context

The same binary runs in very different environments: a laptop dev box,
a headless server VM, and a Pi frame with no keyboard. systemd
deployments need environment overrides without editing files; humans
need a readable file with comments.

## Decision

Three-tier precedence, lowest to highest:

1. Built-in defaults (`config.Default()`),
2. YAML file passed via `--config`,
3. Environment variables prefixed `OPENFRAME_`.

Env names are the YAML path uppercased with `.`/`-` → `_`:
`frame.sync.manifest_interval` → `OPENFRAME_FRAME_SYNC_MANIFEST_INTERVAL`.
Binding is a small reflection walker over `yaml` tags supporting string,
bool, int, int64, float64, `time.Duration` and string slices — no
external config framework.

`Validate()` is strict on dangerous fields (time windows, driver names,
sizes, IANA time zones) and runs on every load, so a misconfigured box
fails fast at startup rather than misbehaving at 3 a.m.

## Consequences

- `doctor`/`status`/`pair` load configuration *tolerantly* (fall back to
  defaults and report the underlying error) because diagnostics must
  work on a broken machine.
- The env walker is deliberately dumb (no nested lists of objects); if
  config grows beyond flat scalars/windows, reconsider a real framework.
- Durations parse via `time.ParseDuration` ("60m"); window times are
  `HH:MM` + IANA zone, validated with a regex + `time.LoadLocation`.
