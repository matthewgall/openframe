# ADR-0011: Multi-architecture support via pure-Go cross-compilation

**Status:** accepted (CI change: full target matrix on every push)

## Context

The primary deployment target is a Raspberry Pi 3B+ (linux/arm64),
but the server runs fine on any box, developers work on macOS/Windows,
and 32-bit Raspberry Pi OS images (armv6/armv7) remain common. Since
the build is already cgo-free (ADR-0002, ADR-0006), adding targets is
"free" only if the code actually compiles for them — and it did not:
`syscall.Statfs` field widths differ per platform (int32 on 386/arm,
uint32 on darwin, absent on Windows), and the DRM/evdev backends use
Linux-only syscalls.

## Decision

- **Supported matrix** (both binaries, `CGO_ENABLED=0`, verified in CI
  on every push and in `make build-all`):
  `linux/{amd64,arm64,armv6,armv7,386,riscv64}`,
  `darwin/{amd64,arm64}`, `windows/{amd64,arm64}`.
- **Platform seams** (the only OS-specific code in the tree):
  - `internal/platform.FreeBytes` — `unix.Statfs` on Unix,
    `GetDiskFreeSpaceEx` on Windows. Callers (cache quota, updater
    disk check, doctor) go through it; `syscall.Statfs` is banned from
    new code because of the per-platform field-width trap.
  - `internal/platform` evdev name read — ioctl on Linux, no-op
    stub elsewhere.
  - `internal/frame/display/drm` — `//go:build linux`. The DRM/KMS
    backend and its blank-import registration are Linux-only; the
    factory's `auto` mode simply finds no hardware backend elsewhere.
  - `internal/updater` process-group attrs — `Setpgid` on Unix,
    `CREATE_NEW_PROCESS_GROUP` on Windows.
- **Behavioural contract off-Linux**: frame and server run fully
  (headless display, no touch input); `doctor` reports the platform
  fallback instead of probing `/dev/dri`. The auto-updater is a Linux
  deployment tool in practice, but compiles everywhere so a single
  source tree is the whole story.
- **CI**: one cross-compile job builds the whole matrix in a single
  warm-cache runner (20 small binaries ≈ 1–2 min) instead of a
  per-arch matrix of runners (N× checkout+setup overhead); the
  host-arch binary is smoke-tested, the rest are verified by
  compilation.

## Consequences

- Adding a target = one line in CI + Makefile + this list, provided
  the build passes; removing platform support would mean deleting the
  seam files, which is why the seams are kept tiny.
- Linux-only features must go behind build tags from the start —
  a new `syscall` use without a platform file fails 8 of 10 CI
  builds immediately, which is exactly what we want.
- The updater's Windows path (SIGTERM→Kill fallback) is
  compile-verified but not behaviour-tested; it is not a deployment
  target, and the systemd service control remains Linux-only.
