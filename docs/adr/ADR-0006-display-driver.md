# ADR-0006: Display driver boundary; DRM/KMS leading for M4; headless dev renderer

**Status:** implemented (milestone 4)

## Context

The frame must render a slideshow/gallery on a 1280×800 HDMI panel with
USB touch, on Raspberry Pi OS **Lite** (no X11, no desktop), while
development and CI run on headless Linux AMD64 boxes. The spec requires
evaluating SDL2 vs DRM/KMS, a development renderer without hardware,
and a backend that never leaks into the server binary.

## Decision

- `internal/frame/display` defines the `Driver` interface:
  `Name/Info/Init/Close/Present/SetBrightness/SetPower`. Everything
  above the renderer (agent, sync, sleep scheduler, settings) talks
  only to this interface.
- **M1 ships the headless driver** (the dev renderer and the AMD64
  test simulator): in-memory state, no hardware, implements the
  documented fallbacks (brightness/power accepted, no physical effect).
- **M4 selection: DRM/KMS is the leading candidate** for the hardware
  driver, because Pi OS Lite has no display server and KMS gives direct
  mode setting, page-flip rendering and atomic commit without X11.
  SDL2 remains the fallback option (wider ecosystem, but drags in an
  X11/video subsystem and larger memory). The decision is confirmed
  with real-hardware validation in M4 (EDID/mode, power draw, touch
  coexistence) and recorded in `docs/hardware/`.
- Hardware drivers (and their cgo deps, if any) sit behind build tags
  so the default cross-build stays `CGO_ENABLED=0`.
- `frame.display.driver`: `headless` (dev default), `auto` (probe
  hardware, fall back to headless), `drm` (M4).
- Backlight: never assumed. `SetPower`/`SetBrightness` degrade to
  no-op/black-frame where the hardware exposes no control, per spec §11.

## M4 implementation notes

- **DRM/KMS won.** `internal/frame/display/drm` implements the hardware
  backend in pure Go (`golang.org/x/sys/unix` only): no X11, no Wayland,
  no SDL, no cgo — the `CGO_ENABLED=0` cross-build property (ADR-0002)
  is intact. Scope is deliberately narrow: single connector, single
  primary plane, one mode, XRGB8888 dumb (CPU) framebuffers, atomic
  commits. That covers the target panel and everything the frame does.
- **ABI verified against kernel headers.** Every ioctl number and
  struct layout in `drm/ioctls.go` was derived from the Linux uapi
  headers (v5.10, v6.6 and master all agree) and pinned by
  `layout_test.go`, which asserts the mirrored Go struct sizes and the
  exact ioctl request words at build time. A future layout slip fails
  CI instead of a panel.
- **Registration pattern.** The display factory cannot import the drm
  package (the drm package implements `display.Driver` and imports
  `display`). The backend registers itself with `display.Register` from
  `init()`; the frame agent pulls it in with a blank import. "auto"
  tries registered backends in order, then falls back to headless.
- **Probe → commit split.** `New()` only probes (card, connected
  connector, preferred mode, plane, property names). `Init()` applies
  the mode with the first atomic commit (ALLOW_MODESET) and a black
  frame. `PresentFrame()` copies the UI canvas into the dumb buffer,
  wraps it in a fresh framebuffer and commits; the previous framebuffer
  is released after the commit lands. Failure leaves the last frame on
  screen (KMS holds the previous framebuffer).
- **Brightness/power via sysfs backlight**, never assumed: frames
  without a backlight degrade to no-op (spec §11).
- **Headless driver = visual simulator.** With `snapshot_dir` set, the
  headless driver writes `latest.png` + a bounded timestamped ring of
  every presented frame, so the entire UI is testable and observable on
  dev boxes without hardware (spec §10, §20).
- **Residual risk (unchanged from the decision):** the specific
  Waveshare panel's EDID/KMS behaviour is validated on real hardware
  before release; until then `driver: auto` degrades to headless and
  `doctor` reports the probe result. The unit-testable layers (struct
  layouts, mode selection, property matching, gesture recognition, UI
  state machine, compositing) are covered by tests.
