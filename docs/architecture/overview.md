# OpenFrame — Architecture Overview

## System

```
                    ┌────────────────────────────┐
                    │   Management server        │
  Family members ──▶│  (openframe server)        │
  (browser uploads) │  - auth, users, albums     │
                    │  - photo storage (FS/S3)   │
                    │  - manifests, releases     │
                    └─────────────┬──────────────┘
                                  │ HTTPS (outbound from frames only)
                    ┌─────────────┴──────────────┐
        ┌───────────┤  Frame agent               ├───────────┐
        │           │  (openframe frame)         │           │
        │           │  - identity, pairing       │           │
        │           │  - manifest + download sync│           │
        │           │  - local SQLite + cache    │           │
        │           │  - health reporting        │           │
        │           │  - update coordination     │           │
        │           └─────────────┬──────────────┘           │
        │                         │                          │
   ┌────┴─────┐            ┌──────┴───────┐         ┌────────┴─────────┐
   │ Renderer │            │ Content-     │         │ openframe-updater│
   │ (slideshow│            │ addressed    │         │ (separate binary,│
   │ /gallery) │            │ photo cache  │         │ M6)              │
   └──────────┘            └──────────────┘         └──────────────────┘
```

## Process model

- **Server** (`openframe server`): one process; HTTP API + embedded admin
  SPA + SQLite + storage backend. Never links display/graphics code.
- **Frame** (`openframe frame`): one process containing the *agent*
  (identity, sync, health, update coordination) and the *renderer*
  (display + touch + UI). They are separated components in a bounded
  runner: a renderer failure cannot corrupt sync state, and a sync
  failure cannot take the display down.
- **Updater** (`openframe-updater`, M6): separate root-owned binary that
  manages A/B binary slots. The frame requests updates; the updater
  performs verification, installation and rollback. It is independently
  restartable and recovers from a failed application release.

## Server internals (M2)

```
internal/server
├── server.go        lifecycle: DB, storage bootstrap, HTTP, graceful shutdown
├── api/             REST routes, middleware (session auth, CSRF, RBAC), handlers
├── auth/            argon2id hashing, hashed DB sessions, CSRF derivation, rate limiter
├── users/           user records, roles, last-active-admin lockout
├── photos/          upload pipeline (validate → orient → hash → derivatives), photo store
├── albums/          albums, membership, covers, device assignment
├── devices/         M2 minimal device registry (pairing/credentials in M5)
├── storage/         Storage interface + content-addressed local FS backend
└── audit/           append-only audit recorder (JSON detail, no secrets)
```

The API layer is the only HTTP-facing code; domain packages are plain
stores over the shared `*sql.DB` and are individually testable against
the real migrations. The storage backend is an interface so M7 can add
an S3-compatible implementation without touching the API.

## Offline-first invariants

The frame must survive indefinite loss of the server:

1. All displayed photographs live in a local content-addressed cache.
2. The *active collection* (albums + photos) is a revisioned,
   transactionally-activated set. A new manifest never becomes active
   until every mandatory file is downloaded and SHA-256-verified.
3. The previous working collection is retained until the new one is
   safely activated; garbage collection never touches files referenced
   by the active collection.
4. Local changes (favourites) are queued offline and applied idempotently
   when connectivity returns.
5. Display, gallery and sleep scheduling run entirely from local state.
   An unreachable server only blocks *new* content and health reporting.

## Storage

- **Server**: SQLite for all metadata; originals stored in a storage
  backend behind an interface (local filesystem first, S3-compatible
  second). Immutable content-addressed objects; derivatives
  (display-resolution + thumbnails) are regenerable and never the source
  of truth.
- **Frame**: SQLite for identity, manifest revisions, download queue,
  favourites, sync history; content-addressed files for photographs.
  Downloads go to temp files, are hash-verified, then committed.

Both sides use pure-Go SQLite (modernc.org/sqlite) so every binary
cross-compiles without a C toolchain (ADR-0002).

## Database compatibility (rollback rule)

Binary rollback (M6) is only safe when the new binary can read the old
database and vice versa. Rules:

1. Migrations are **additive and backward-compatible**: add columns with
   defaults, add tables, soft-delete. No dropping or redefining of
   in-use columns, no schema changes that require rewriting data.
2. A release that needs an incompatible migration must (a) ship the
   migration in a *separately versioned* step that old binaries ignore
   (feature-flag the read path), or (b) require a verified backup +
   tested restore before the update is permitted. The updater rejects
   the update otherwise.
3. Migration files are checksummed; editing a shipped migration is a
   hard error at startup.

## Synchronisation protocol (M3 detail, direction fixed now)

- Frame → server: `GET {server}/api/v1/frames/{device}/manifest` with
  `If-None-Match: <etag>`; server replies `304` or a manifest
  (`openframe-manifest/1`) containing revision, album assignments,
  photo identifiers, SHA-256 hashes, sizes, and authenticated resource
  references (never permanent object-storage URLs).
- Frame downloads referenced files, verifies hashes, then activates the
  collection transactionally in local SQLite.
- Priority delivery may bypass the download schedule but never security
  checks.

## Update trust (M6 detail, direction fixed now)

- Ed25519-signed release metadata; trusted public keys are pinned in
  `trust/trusted-keys.json` on the device, independent of the release
  server (ADR-0007).
- Metadata carries version, channel, arch, size, SHA-256, signature,
  minimum compatible versions, timestamp and expiry.
- Anti-rollback: versions are monotonic per channel; downgrade attempts
  are rejected unless explicitly signed as a rollback release.
- A/B slots: the running slot is never overwritten; the candidate is
  installed into the inactive slot, verified, activated atomically,
  health-checked, and committed — with automatic rollback to the last
  known-good slot on failure, including across power loss.

## Display backend (M4 decision, boundary fixed now)

The renderer talks to a `display.Driver` interface
(`internal/frame/display`): `Init/Close/Present/SetBrightness/SetPower`.
M1 ships the headless development renderer (also the AMD64 test
simulator). M4 will add a hardware driver — DRM/KMS is the leading
candidate for Pi kiosk use (no X11, direct KMS flip, works on Pi OS
Lite) — selected via `frame.display.driver: drm` or `auto`
(ADR-0006). Backlight control is behind the same interface with a
documented fallback (black frame + reduced blanking) where the hardware
exposes no backlight switch.

## Resource targets (Pi 3B+, 1 GB RAM)

- Idle frame memory < 200 MB (measured, not assumed; tracked from M4).
- No Electron, no mandatory Chromium, no desktop environment.
- Bounded image decoding (decode at target resolution, not native).
- Bounded goroutines/queues (component runner caps concurrency).
- No continuous redraws; slideshow advances on a timer, gallery only on
  interaction; the admin UI stops polling when hidden.

## Failure behaviour summary

| Failure | Behaviour |
|---|---|
| Server down | Frame keeps displaying; sync deferred with backoff |
| Download interrupted | Temp file discarded/retained per resume policy; previous collection untouched |
| Bad hash | File rejected; retry with backoff; previous collection untouched |
| Disk full | Downloads stop cleanly; cache and active collection intact |
| Renderer crash | Component runner restarts renderer; sync state unaffected |
| Bad new binary | Updater rolls back to previous slot (no internet needed) |
| Power loss during update | Updater recovers to last committed slot on next boot |
| SD card corruption | `doctor` reports; frame identity + cache are the recovery assets |
