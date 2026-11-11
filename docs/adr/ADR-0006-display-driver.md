# ADR-0006: Display driver boundary; DRM/KMS leading for M4; headless dev renderer

**Status:** accepted (milestone 1); hardware backend decision finalised in M4

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

## Consequences

- The server binary links no graphics code at all (verified by
  dependency isolation: `internal/server` has no display imports).
- Renderer crash isolation: the renderer is one component in the frame's
  bounded runner; it cannot corrupt sync state.
- M4 carries the residual risk that the specific Waveshare panel
  misbehaves under KMS (EDID quirks); the headless driver keeps the
  whole system testable regardless.
