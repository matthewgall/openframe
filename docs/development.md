# Development Guide

## Prerequisites

- Go ≥ 1.26 (`go version`)
- Node ≥ 20 (only for rebuilding the admin SPA; the built assets are
  committed, so a plain Go build needs no Node)
- Linux (amd64 or arm64) for development. Tests use real temp SQLite
  databases and the real filesystem; there is no mock-persistence mode.

## First build & test

```bash
make build        # bin/openframe
make test         # unit + integration tests
make test-race    # with the race detector
make vet          # go vet
make fmt          # gofmt -w
```

The whole suite runs in a few seconds on a laptop. `make all` runs
fmt-check, vet, test and build in that order.

## Running locally

```bash
make build
make run-server   # :8080, state under ./.data-dev
make run-frame    # headless agent, same state dir
make doctor
make status
```

Both use the state directory `.data-dev` by default when run through the
Makefile. Override with `OPENFRAME_DATA_DIR`.

The admin UI is at http://localhost:8080. Library, albums, users,
frames and audit are live (M2/M3); dashboard shows live stats.

## Frame UI development (M4)

The frame UI has no window of its own: it composites into a
full-screen canvas and hands pixels to a display driver. Without a
panel, use the **headless visual simulator**:

```yaml
frame:
  display:
    driver: headless
    width: 1280
    height: 800
    snapshot_dir: /tmp/of-snap    # writes latest.png + a small ring
```

`openframe frame` now renders the entire interface to
`/tmp/of-snap/latest.png` on every present — open it in any image
viewer and you can see the slideshow, gallery, search, settings and
empty states, and watch sync activations refresh the display. The M4
end-to-end test (`internal/frame/e2e_test.go`) does exactly this:
real server → real frame → decodes the snapshot → asserts the
uploaded photo's pixels are on screen.

UI development conventions:

- `internal/frame/ui` is a synchronous state machine: one goroutine
  (the engine's `Run`) owns all state; external updates
  (`Reload`/`SetAsleep`/`SetThrottled`) take the engine mutex briefly.
  Tests drive it directly with `HandleGesture`/`Tick` and an injected
  clock — no real time, no hardware.
- The touch recognizer (`internal/frame/touch`) is pure: feed it
  synthetic `Sample`s and assert `Gesture`s. evdev parsing is covered
  with byte buffers.
- The DRM ioctl layer is byte-for-byte pinned to the kernel uapi by
  `internal/frame/display/drm/layout_test.go` (struct sizes + request
  words). It cannot be exercised without a panel; on hardware, start
  with `openframe doctor` (it probes the exact same code path).

## Working on the admin SPA

```bash
make web-install  # npm install (once)
make web-dev      # Vite dev server on :5173, proxies /healthz + /api to :8080
make web          # production build into web/admin/dist
```

The built `web/admin/dist/` is **committed** and embedded into the Go
binary via `web/embed.go`. After changing SPA source, run `make web` and
commit the regenerated `dist/` alongside the source change so the Go
binary always embeds the UI you tested. CI verifies the committed dist
matches a fresh build (warning on drift).

The SPA is framework-free TypeScript. Keep it that way unless a
framework earns its place in a review — the constraint is intentional
(see ADR-0005).

## Configuration

See the "Configuration" section of the README. Quick reference for env
overrides:

| Key | Env var |
|---|---|
| `server.listen` | `OPENFRAME_SERVER_LISTEN` |
| `frame.server_url` | `OPENFRAME_FRAME_SERVER_URL` |
| `log.level` | `OPENFRAME_LOG_LEVEL` |
| `frame.display.driver` | `OPENFRAME_FRAME_DISPLAY_DRIVER` |
| `frame.sync.policy` | `OPENFRAME_FRAME_SYNC_POLICY` |
| `data_dir` | `OPENFRAME_DATA_DIR` |

The full mapping is mechanical: `a.b.c` → `OPENFRAME_A_B_C`.

## Adding a migration

1. Create `migrations/server/NNNN_description.sql` (or `migrations/frame/`).
   `NNNN` is the next integer in that subset.
2. The file is embedded at build time; no registration step.
3. Keep it additive and backward-compatible (see the "Database
   compatibility" section of the architecture doc). Editing a shipped
   migration is rejected at runtime by checksum.
4. Add a test that exercises the new schema.

Migrations run automatically on `openframe server` and `openframe frame`
startup, in version order, each in its own transaction.

## Adding a CLI command

Commands live in `internal/cli/`. The dispatcher (`cli.go`) is a flat
list of `command{name, short, run}` entries. Add a file
`cmd_<name>.go` with a `run<Name>(args []string) int` function, return
`ExitOK`/`ExitError`/`ExitUsage`, and register it in `commands`.

Keep commands side-effect-light: long-running work belongs in the
`server` and `frame` commands; `doctor` and `status` must be safe to run
anywhere, including a broken production box, and must never reveal
secrets.

## Cross-compiling for the Pi

```bash
make build-all   # linux/amd64 + linux/arm64 into dist/
```

or manually:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o openframe-arm64 ./cmd/openframe
```

No cross C toolchain is needed (pure-Go SQLite). The frame binary must
remain free of cgo graphics dependencies until a hardware display driver
is added in M4; even then, keep it behind a build tag so the default
cross-build stays toolchain-free (ADR-0006).

## Testing conventions

- Integration tests use `t.TempDir()` and real SQLite files.
- Long-running commands (`server`, `frame`) are tested by starting them
  in a goroutine with a cancellable context and asserting on
  readiness/shutdown, not by waiting for real time.
- Display tests use the headless driver — it is the simulator for frame
  behaviour on Linux AMD64 (spec §20).
- Race detector is part of CI; local `make test-race` is the equivalent.

## What is deliberately not here yet

- Multi-frame management polish (M5): in-UI pairing challenge/approval
  (today: admin creates the device with the frame's identity + issues
  a credential), remote sync requests, per-frame configuration from
  the admin UI, health reporting.
- Auto-updater (M6) — `internal/updater/` is an empty boundary package.
- Production hardening (M7): backups/exports, long-run soak,
  security review pass.
