# ADR-0012: S3-compatible photo storage

- **Status**: accepted (revised 2026-11-14)
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
new dependency — and the SDK chosen below satisfies it.

## Decision

### 1. The official AWS SDK for Go v2 (`github.com/aws/aws-sdk-go-v2`)

The S3 backend is implemented on `aws-sdk-go-v2/service/s3` (with
`aws-sdk-go-v2` core + `credentials` for static credentials). Both are
pure Go with no cgo, so the 10-target cross-compile matrix and the
`CGO_ENABLED=0` invariant (ADR-0002) are unaffected.

**Revision note (2026-11-14, same day):** this ADR was originally
written with a hand-rolled SigV4 signer + thin HTTP client (stdlib
only, no cloud SDK), on the grounds that the operation set is small
(4 verbs) and dependency weight was worth avoiding. After review it
was replaced by the official SDK. The SDK is the battle-tested
reference implementation of the S3 wire protocol: it knows the
provider quirks (addressing rules, checksum semantics, retry and
throttle behaviour, error shapes) that thousands of deployments have
hammered into it, and it is what AWS's own tooling ships. A hand-
rolled client, however well tested, would have to re-learn that
long tail (e.g. HTTPS PUTs legitimately signed with
`UNSIGNED-PAYLOAD`, 416 error shapes, non-conformant providers) every
time behaviour drifts. The dependency cost is acceptable: the module
tree is well-maintained, pure Go, and stays within the
`govulncheck`-monitored dependency set.

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
      path_style: true     # R2/MinIO; leave false for AWS S3
      prefix: ""           # optional namespace inside the bucket
```

`endpoint` empty → AWS (the SDK derives the regional endpoint from
`region`). Env overlay works per-field
(`OPENFRAME_SERVER_PHOTO_STORAGE_S3_*`).

### 3. Hermetic AWS configuration

The `aws.Config` is built from OpenFrame config only — **static
credentials and the explicit region**. No env-var discovery, no
shared-credentials files, no IMDS probing. The server talks to
exactly the store named in `openframe.yaml` and nothing else, which
matters for a box that also serves frames (T-06) and for predictable
failures.

### 4. The Storage contract is the seam

`S3Storage` implements the same interface with the same semantics as
`LocalFS`:

- `Put` buffers the body, verifies content-addressed originals
  against their key (the key IS the SHA-256 of the bytes), HEADs the
  key and skips if the object already exists (LocalFS parity — an
  existing content-addressed object is never rewritten).
- `OpenAt` uses a ranged GET; a 416 (offset past end) maps to an
  immediate-EOF reader, matching `LocalFS` seek-past-EOF behaviour.
- 404 (`NoSuchKey`/`NotFound`) maps to the shared `ErrNotFound`.
- Transient failures retry with the SDK's standard retryer
  (3 attempts, jittered backoff); every operation is idempotent.
- `TotalUsage` uses the `ListObjectsV2` paginator (30s cache, same as
  local).

### 5. Scope limits (v1)

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
- Signing, addressing and retries are the SDK's responsibility. Test
  coverage lives at the `Storage` interface boundary: the in-test S3
  server validates the SigV4 shape of every request the SDK produces
  (Authorization layout, `X-Amz-Date`, payload hash — including the
  legitimate `UNSIGNED-PAYLOAD` form used over TLS), and a live e2e
  runs the real API + pipeline against it.
- The SDK is a large surface for CVEs to land in: it is covered by
  `govulncheck` in CI and by dependabot's Go ecosystem, and the
  pinned versions are visible in `go.mod`.

## Related

- ADR-0002 (pure Go, no cgo — the SDK satisfies this)
- ADR-0003 (config layering — env overlay for secrets)
- `docs/deployment/production.md` (R2 setup + backup scope)
- `docs/security/threat-model.md` (T-06: credentials at rest)
