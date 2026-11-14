# ADR-0012: S3-compatible photo storage with hand-rolled SigV4

- **Status**: accepted
- **Date**: 2026-11-14
- **Supersedes**: nothing (extends the storage seam introduced in M2)

## Context

Photo originals and derived variants are stored as content-addressed
objects (SHA-256 keys, plus `.display`/`.thumb`/... suffixes). The
`storage.Storage` interface abstracts the backend so the server never
cares where bytes live. v1 shipped with `LocalFS` only.

For production deployments (family scale: tens of thousands of photos,
many GB of derivatives), a durable off-box object store is strongly
preferred:

- **Durability**: a single SSD failure takes the whole library with it.
  S3-class stores offer 11-nines durability.
- **Economics**: object storage is far cheaper per GB than the SSD
  capacity a photo library would otherwise consume on the server box.
- **Cloudflare R2 specifically**: zero egress fees (the server reads
  originals/derivatives constantly — egress on classic S3 would be
  meaningful), simple SigV4-compatible API, path-style addressing.

The constraint set from ADR-0002 (pure Go, `CGO_ENABLED=0`,
toolchain-free cross-compilation to 10 targets) still applies to any
new dependency.

## Decision

### 1. Hand-rolled AWS SigV4, no cloud SDK

`internal/server/storage/sigv4.go` implements AWS Signature Version 4
using only `crypto/hmac` + `crypto/sha256` (stdlib). No AWS/Cloudflare
SDK, no third-party sigv4 library.

Rationale:

- **Dependency weight**: an SDK (aws-sdk-go-v2) pulls a large module
  tree into a server whose other dependencies are deliberately small.
  The subset of S3 we use is PUT / GET (+Range) / HEAD / ListObjectsV2
  — four HTTP verbs over a well-specified wire format.
- **Correctness is testable**: the signer is validated against the
  official AWS test-suite vector (`get-vanilla`) plus a query-ordering
  vector cross-verified against an independent SigV4 implementation,
  and the in-test S3 server validates the shape of every request
  (Authorization layout, X-Amz-Date, payload hash == body hash) so
  regressions fail loudly.
- **Portability**: nothing in it constrains the cross-compile matrix.

The signer is deliberately minimal: it signs HTTP requests for the
`s3` service with query-string auth disabled, which is all this
application needs.

### 2. One backend, many stores

`S3Storage` targets the S3 API generically: AWS S3, Cloudflare R2,
MinIO, Garage, Ceph, and similar. Configuration:

```yaml
server:
  photo_storage:
    backend: s3
    s3:
      endpoint: https://<account>.r2.cloudflarestorage.com
      region: auto
      bucket: family-photos
      access_key_id: ...
      secret_access_key: ...
      path_style: true     # R2/MinIO; AWS S3 uses virtual-host
      prefix: ""           # optional namespace inside the bucket
```

`endpoint` empty → `https://s3.<region>.amazonaws.com` (AWS). Env
overlay works per-field (`OPENFRAME_SERVER_PHOTO_STORAGE_S3_*`).

### 3. The Storage contract is the seam

`S3Storage` implements the same interface with the same semantics as
`LocalFS`:

- `Put` re-hashes the upload in memory (it must hold the bytes for
  the `X-Amz-Content-Sha256` header anyway) and verifies
  content-addressed originals against their key; it then HEADs the
  key and skips if the object already exists (LocalFS parity — an
  existing content-addressed object is never rewritten).
- `OpenAt` uses `Range: bytes=N-`; a 416 (offset past end) maps to an
  immediate-EOF reader, matching `LocalFS` seek-past-EOF behaviour.
- 404 maps to the shared `ErrNotFound`; retryable errors (network,
  503, 5xx) retry with short backoff (250ms/1s, 3 attempts) because
  every operation is idempotent.
- `TotalUsage` lists with `ListObjectsV2` (paginated, 30s cache,
  same as local).

### 4. Scope limits (v1)

- **No server-side encryption config** (SSE) beyond what the bucket
  defaults to; bucket-level SSE remains operator policy.
- **No multipart uploads**: the 12MP pipeline caps uploads at
  `max_upload_bytes` (default 50 MiB) and buffers the request in
  memory for hashing — a single PUT is the only upload path needed.
- **No lifecycle rules** (originals vs derivatives tiers) from the
  application; bucket policy remains operator configuration.
- **Backups**: with `backend: s3`, `openframe backup server` still
  snapshots `server.db` (catalog, users, frames, releases) but does
  **not** download objects; the backup records a `storage_note` in
  `backup-info.json` saying the objects live in the bucket. The
  bucket's own durability/versioning is the object backup. See
  `docs/deployment/production.md`.

## Consequences

- `server.photo_storage.backend` selects `local` (default, zero
  behaviour change) or `s3`.
- `openframe doctor` gains a photo-storage check (local dir presence,
  S3 reachability via a one-key ListObjectsV2).
- New code paths are all behind the existing `Storage` interface; the
  upload pipeline, manifest, and frame sync are backend-agnostic.
- SigV4 signing is a new cryptographic surface: it is covered by
  vector tests in `internal/server/storage/sigv4_test.go` and by the
  request-shape validation in `s3_test.go`. It should be re-reviewed
  if the signed-operation set grows beyond PUT/GET/HEAD/LIST.

## Related

- ADR-0002 (pure Go, no cgo — constrains dependency choices)
- ADR-0003 (config layering — env overlay for secrets)
- `docs/deployment/production.md` (R2 setup + backup scope)
- `docs/security/threat-model.md` (T-06: credentials at rest)
