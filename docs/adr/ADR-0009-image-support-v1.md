# ADR-0009: v1 image support — JPEG/PNG/WebP; HEIC deferred

**Status:** accepted (milestone 1); HEIC is a documented extension point

## Context

The server must validate uploads, apply EXIF orientation, and generate
display-resolution derivatives + thumbnails. HEIC (iPhone default) is
requested "where supported by available image-processing dependencies."
Realistic HEIC decoding in Go is cgo + `libheif` (goheif), which
breaks the pure-Go, toolchain-free cross-build that ADR-0002 depends on
unless isolated behind a build tag.

## Decision

- **v1 supports JPEG, PNG and WebP** (decode + re-encode for
  derivatives), implemented with the standard library +
  `golang.org/x/image` (draw/scaling) and `chai2010/webp` (WebP
  encode). All in the server path, M2.
- **HEIC uploads are rejected with HTTP 415** and a clear message in
  v1. The decoder sits behind a small `image.DecodeImage(r) (img, meta,
  err)` seam with a registry of format handlers, so adding a HEIC
  handler (cgo build tag + libheif) later does not touch the upload
  pipeline.
- EXIF orientation is applied at derivative generation; originals are
  preserved untouched. Sensitive EXIF (GPS) is stripped from
  frame-delivered derivatives.

## Consequences

- The server stays pure-Go and cross-compiles everywhere in v1.
- iPhone users must convert (many phones/cameras now export JPEG by
  default; the 415 message says exactly how to proceed).
- The HEIC extension point is a build-tagged handler + one dependency;
  if demand is real, it is a contained M2+ change, not a re-architecture.
