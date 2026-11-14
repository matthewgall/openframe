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

## Server internals (M2–M3)

```
internal/server
├── server.go        lifecycle: DB, storage bootstrap, HTTP, graceful shutdown
├── api/             REST routes, middleware (session auth, CSRF, RBAC, device auth), handlers
├── auth/            argon2id hashing, hashed DB sessions, CSRF derivation, rate limiter
├── users/           user records, roles, last-active-admin lockout
├── photos/          upload pipeline (validate → orient → hash → derivatives), photo store
├── albums/          albums, membership, covers, device assignment
├── devices/         M3 credential lifecycle + M5 health store, sync-request queue,
│                    per-frame config (versioned, validated)
├── pairing/         M5 pairing store: short codes (hash-only at rest), rate limits,
│                    approval → device + one-time credential claim
├── manifest/        frame manifest builder (content revision + M5 config revision)
├── storage/         Storage interface + content-addressed local FS backend (Range reads
│                    for resume) + S3-compatible backend (hand-rolled SigV4, ADR-0012)
└── audit/           append-only audit recorder (JSON detail, no secrets)
```

## Frame internals (M4–M5)

```
internal/frame
├── frame.go            lifecycle: identity, DB, cache, sync engine, UI stack, components
├── ui_wiring.go        M4 display stack: UI engine, touch, sleep, thermal, ui.Data
├── settings.go         local settings overrides (frame.db frame_settings)
├── credential.go       M5: 0600 credential file (atomic temp+rename, not in frame.db)
├── pairing_wiring.go   M5: unpaired loop — challenge → code on screen → poll → claim
├── effective.go        M5: effective config (config file < server config < local)
├── health.go           M5: 30 s health report, bounded error ring, sync-request seq
├── pairing/            M5 pairing client (challenge request, status poll, claim)
├── cache/              content-addressed derivative cache (.part → verify → rename; quota + GC)
├── sync/               sync engine: manifest checker, download queue/worker, atomic
│                       activation, offline favourites queue, backoff + jitter, policies,
│                       M5: runtime SetCredential + OnConfig hooks
├── display/            driver boundary + headless visual simulator
│   └── drm/            pure-Go DRM/KMS renderer (atomic commits, dumb buffers)
├── canvas/             fullscreen NRGBA compositing, fit modes, transitions, bitmap font
├── ui/                 touchscreen state machine: pairing, slideshow, details, gallery,
│                       album, search, settings, sleep; gesture dispatch; high-contrast
├── touch/              evdev 24-byte event reader, sample frames, gesture recognizer
├── sleep/              daily on/off schedule (minutes-from-midnight, IANA tz)
└── thermal/            sysfs thermal-zone monitor (fail-open)
```

### Display pipeline

```
frameData (ui.Data)  →  ui.Engine  →  canvas (composited NRGBA)
  reads frame.db        state machine     photo cache + overlays
  active collection     gestures/ticks    → Driver.PresentFrame
                                              ├── headless (simulator: PNG snapshots)
                                              └── drm (KMS atomic commit → panel)
```

The display path reads only the *active* collection; the sync engine
writes *staging* state and promotes it in one transaction, so an
incomplete sync is invisible to the display. Sync activation calls the
UI's `Reload` through a hook; the UI re-reads the collection on its
own thread (the engine's state mutex makes cross-goroutine updates
safe) and presents the new set.

### Touch and safety

- Touch input: evdev → sample frames → gesture recognizer
  (tap / double-tap / swipe / hold) → bounded (32) gesture channel →
  UI. No touchscreen: the frame runs touchless (everything is
  navigable by timer; settings apply is hold-protected).
- Sleep: the schedule turns the *display* off (backlight, or black
  frame) while the agent keeps running and syncing (spec §11).
- Thermal: above the threshold the slideshow interval doubles; a
  missing sensor fails open (never throttles on a read error).
- Local settings (brightness, interval, transition, orientation,
  sleep window, captions, clock) persist in frame.db and survive
  restarts; they override the config file for that frame.

### Pairing and multi-frame (M5)

**Pairing protocol** (no inbound connections, no typed tokens):

```
frame                                   server                     admin
 │ POST /devices/pair-challenges {id, name} │                        │
 │────────────────────────────────────────►│ create challenge        │
 │ ◄─────────────────────────────────────── │ 8-char code (SHA-256   │
 │ code shown on the pairing screen         │ hash stored; rate      │
 │                                          │ limited per source IP) │
 │                                          │◄── pending list ───────│
 │                                          │ POST approve {code} ───│
 │ POST /devices/pair-status {challenge_id} │ code verified (constant│
 │────────────────────────────────────────►│ time) → device + token │
 │ ◄─────────────────────────────────────── │ credential returned    │
 │ credential claimed once (server nulls    │ exactly once; frame    │
 │ it after first claim) → saved to 0600   │ saves it to disk and   │
 │ file → sync engine SetCredential → sync │ starts syncing         │
```

The server never initiates connections; the frame pulls everything
(pairing status, manifest, downloads). Challenge codes are short-lived,
single-use and brute-force-resistant (rate-limited creations +
constant-time comparison; see the threat model).

**Health reporting**: every 30 s the frame posts a sanitized snapshot
(version, uptime, last sync, manifest revision, disk, memory, CPU
temperature, display driver/status, bounded recent errors). The server
keeps only the latest report per device and derives the status shown
in the admin UI: `online` (recent heartbeat) → `recently_seen` →
`offline`, with `sync_error`, `updating`, `update_failed`, `rollback`
overlays from the report. The same response carries the
`sync_request_seq`: when an admin **triggers a sync**, the frame sees
the sequence increase on its next heartbeat and runs an immediate
check — the only "remote control" path, and it rides the frame's own
outbound request.

**Per-frame configuration**: the admin UI stores a versioned JSON
document per device (validated server-side; bad input is a 400, never
a partial write). It travels in the manifest's `config` block with its
own `config_revision`; the ETag includes both revisions, so a
config-only change re-fetches the manifest without photo churn. The
frame layers it as: **local settings > server config > config file** —
a frame-local adjustment always wins, and clearing it falls back to
the server value. The effective config is applied live (UI interval,
transition, brightness, orientation, sleep schedule, sync interval)
and the pointer swap is mutex-serialized against the UI/render
threads.

The API layer is the only HTTP-facing code; domain packages are plain
stores over the shared `*sql.DB` and are individually testable against
the real migrations. The storage backend is an interface; S3-compatible
backends (AWS S3, Cloudflare R2, MinIO, ...) were added without touching
the API, upload pipeline or frame sync (ADR-0012).

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
