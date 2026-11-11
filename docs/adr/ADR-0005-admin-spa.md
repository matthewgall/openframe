# ADR-0005: Admin UI — Vite-bundled framework-free TypeScript, embedded

**Status:** accepted (milestone 1)

## Context

The management web UI must be responsive (desktop + mobile),
accessible, touch-friendly, and embedded in the Go server binary
(no separate web deployment). The spec allows a Node build step for the
SPA (confirmed with the operator) but values portability and a small
attack surface.

## Decision

- `web/admin` is a Vite + TypeScript app with **no UI framework**
  (vanilla DOM helpers, one CSS file, a tiny typed fetch client).
- `npm run build` emits `web/admin/dist/`, which is **committed** and
  embedded via `//go:embed all:admin/dist` in `web/embed.go`. A plain
  `go build` therefore works with no Node installed; CI rebuilds and
  warns if the committed dist drifts from source.
- Serving: a GET catch-all in the chi router serves static assets,
  falls back to `index.html` for extensionless SPA routes, and returns
  a JSON 404 for `/api/*` so the SPA can never shadow API responses.
  Assets are cached in memory at startup (the UI is well under 1 MiB).

## Consequences

- The Go release pipeline has a soft Node dependency (build the UI) but
  the Go binary itself does not require Node at build time.
- Framework-free keeps the bundle tiny and auditable; if a future
  milestone needs component state management that vanilla TS handles
  badly, adopting a small framework is a contained change to
  `web/admin` only.
- Committed build artifacts are unusual but deliberate: they keep the
  embedded binary reproducible from a fresh clone without installing
  Node, and CI treats them like generated code (drift check).
