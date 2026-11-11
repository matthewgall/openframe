# ADR-0010: Structured logging — log/slog, JSON default

**Status:** accepted (milestone 1)

## Context

An unattended appliance must log enough to diagnose a failure months
later (over SSH or in a serial console) without leaking secrets, and the
logs must be greppable both by humans (`text`) and by tools (`json`).
Go 1.21+ ships `log/slog`, which the spec names explicitly.

## Decision

- `internal/logging` builds the root `*slog.Logger` from config
  (`level`, `format` json|text, `output` stderr|stdout).
- **JSON is the default** (machine-greppable in journalctl); `text` is
  available for interactive development.
- Every component derives a child logger via
  `logging.NewComponent(log, "frame.agent")` etc., adding a single
  `component` attribute. Callers must not nest `component` attributes
  (duplicate JSON keys) — the frame/display wiring is structured so
  each leaf attaches exactly once.
- **Secrets rule** (T-14): no credentials, tokens, signed URLs,
  cookies or PII in log attributes. `doctor` output is secret-free by
  construction.

## Consequences

- Zero logging dependencies.
- Component names are stable tokens for `journalctl` filters and for
  the M5 health "recent error summaries" aggregation.
- Time format in `text` mode is trimmed to milliseconds for
  readability; JSON mode keeps RFC3339.
