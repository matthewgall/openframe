# Performance (M7)

Measured with the committed benchmarks (`make bench`):

- `internal/server/photos` — `BenchmarkProcessJPEG{4032x3024,1280x800}`
  (full pipeline: validate, decode, EXIF, four derivatives)
- `internal/server/manifest` — `BenchmarkBuildManifest/photos_{100,500}`
  (steady-state manifest poll: DB + storage stats, derivative hashes
  cached)

Reference environment: desktop i5-6500T (2.5 GHz), Go 1.26. A Pi 3B+
(ARM, ~4x slower for image work) should be read as roughly 3–4x the
desktop numbers; the design budgets below hold on both.

## Photo pipeline (upload processing)

| Input | Before (M2) | After (M7) | Change |
|---|---|---|---|
| 4032×3024 JPEG (12 MP) | 3.61 s, 774 MB alloc | **1.88 s, 355 MB alloc** | **1.9x faster** |
| 1280×800 JPEG | 0.69 s | **0.68 s** | faster (smaller case) |

What changed:

1. **Redundant full-size copy removed** (`toNRGBA`): the JPEG/PNG/WebP
   decoders already return a zero-origin `*image.NRGBA`, so the
   pipeline was memmoving the entire 12 MP image (≈50 MB) before
   doing anything. That copy now happens only for genuinely
   non-NRGBA decodes (paletted PNG).
2. **Hybrid scaler** (`drawBilinear`): large downscales (>1.5x on any
   axis — the camera-photo path) use `draw.ApproxBiLinear`, a few
   times cheaper than `draw.CatmullRom`; small resizes and upscales
   keep CatmullRom (ApproxBiLinear is measurably *slower* when
   upscaling, and upscale quality matters more there).

Quality note (the documented trade-off): ApproxBiLinear on a large
downscale of fine textures is slightly more alias-prone than
CatmullRom. At the frame's 1280×800-class display size, downscaling a
12 MP source by ~2–6x, the difference is not perceptible in the
derivatives that are actually shown (display 1920 long edge,
portrait/landscape crops, 384 thumb). The swap is a one-line change
in `orient.go` if a future panel makes it visible.

Remaining cost is dominated by the stdlib JPEG **encoder** (four
derivative encodes, ~35% of CPU) — no faster pure-Go encoder is
available without adding a dependency (ADR-0004/T-15: rejected).
Budget: a 12 MP upload costs ~2 s of CPU on desktop, ~6–8 s on a Pi
3B+; uploads are processed serially per request but a request carries
multiple files and the UI reports progress — acceptable for
family-library scale (hundreds to low thousands of photos, uploaded
once).

## Manifest build (frame sync poll)

| Library size | Desktop |
|---|---|
| 100 photos | 9 ms |
| 500 photos | 44 ms (3.5 MB alloc) |

Per-photo work is two storage stats + two cached-hash lookups
(derivative SHA-256s are recorded at ingest in `photo_derivatives`,
so the poll never re-hashes objects). At 5x Pi slowdown a 500-photo
library is ~200 ms per poll — the frame polls every few minutes, so
this is <1% of one CPU second per day. **No optimisation required.**

## ADR-0002 revisit (pure-Go SQLite)

ADR-0002 promised to revisit if M7 benchmarks said otherwise. They do
not: at family-library scale the pure-Go driver is comfortably fast
(manifest build 44 ms/500 photos; backup VACUUM INTO of a 100 MB DB
is a few seconds; `doctor` integrity checks are sub-second). Keeping
the toolchain-free ARM64 cross-compile is worth more than the
performance delta. **Decision stands.**

## Soak (long-duration reliability)

`make soak` runs the full suite three times with the race detector,
then a sustained mixed-load test
(`internal/server/api/soak_test.go`): concurrent uploads, manifest
polls, photo-list reads and health checks for a
bounded duration, asserting the server stays responsive and the sync
round-trip completes without data loss. It is designed to run
unattended (CI does not run it; a local `make soak` is the
long-duration gate before a release).

## How to re-measure

```
make bench    # go test -bench on the photos + manifest packages
make soak     # repeated race suites + sustained load
```

Record new numbers in this file when the hardware or Go toolchain
changes materially (they are hardware-relative, not absolutes).
