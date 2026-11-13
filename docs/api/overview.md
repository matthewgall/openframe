# REST API — Conventions and Endpoint Reference

OpenFrame exposes a versioned REST API under `/api/v1`. Milestones 1–2
surface the health probe plus the full admin API: authentication, users,
photos, albums, devices, audit and stats.

## Conventions

- **Versioning**: path-based (`/api/v1`). Breaking changes bump the
  major version; the old version stays available for at least one
  release cycle. Frames carry `MaxProtocolVersion`/
  `MinProtocolVersion` (see `internal/protocol`) and must ignore
  responses they cannot parse rather than crash.
- **Auth (admin API, M2)**: sessions are `HttpOnly` `SameSite=Lax`
  cookies (`of_session`, `Secure` when the configured public URL is
  HTTPS). The token is 32 random bytes; only its SHA-256 is stored. All
  mutating requests require the `X-CSRF-Token` header — the
  synchronizer token is derived from the session token (server-side
  recomputable, never transmitted separately). Login is the only
  mutating endpoint exempt from CSRF; it is protected by `SameSite`
  plus per-IP and per-account rate limiting (T-09).
- **Errors**: JSON error body with a stable machine `error` code and a
  human `message`:

  ```json
  { "error": "not_found", "message": "no such api endpoint" }
  ```

  Unknown `/api/*` paths return this shape with `404` — the SPA must
  never shadow API routes (enforced in `web/embed.go` and tested).
- **Idempotency**: frame→server mutations (favourites, health reports)
  are idempotent operations keyed by client-generated identifiers, so
  offline-replayed requests are safe.
- **Conditional requests**: manifest endpoints support `ETag` +
  `If-None-Match` and answer `304` (M3).
- **No permanent object URLs**: photo downloads are authenticated
  resource references, not long-lived storage URLs.
- **Limits**: uploads are size-capped and content-validated (decoded
  pixels, not just magic bytes); rate limiting applies to auth and
  upload endpoints.

## Milestone 1

### `GET /healthz`

Liveness/readiness probe used by load balancers, the admin UI, frames
and `openframe doctor`.

Response `200` (healthy) / `503` (degraded, e.g. database unreachable):

```json
{
  "status": "ok",
  "version": "0.1.0-dev",
  "commit": "abc1234",
  "started_at": "2026-01-01T00:00:00Z",
  "uptime_seconds": 1234,
  "time": "2026-01-01T00:20:34Z",
  "db": { "status": "ok" }
}
```

`Cache-Control: no-store` is always set.

## Milestone 2 — admin API

All endpoints below require a session except `POST /auth/login`.
All mutating endpoints additionally require a valid `X-CSRF-Token`.
Routes marked **admin** require the `admin` role.

### Auth

| Method & path | Purpose | Notes |
|---|---|---|
| `POST /api/v1/auth/login` | Sign in | Body `{email, password}`. Sets the session cookie; returns `{user, csrf_token, expires_at}`. Uniform `401 bad_credentials` for unknown email and wrong password (same argon2 timing). Rate-limited per IP (`server.login_rate_limit_per_min`, default 5/min) and per account (10/min); `429 rate_limited`. |
| `GET /api/v1/auth/csrf` | Current CSRF token | Returns `{csrf_token}`. |
| `POST /api/v1/auth/logout` | Revoke this session | Clears the cookie. |
| `GET /api/v1/auth/me` | Current identity | `{user, session}`. |
| `POST /api/v1/auth/password` | Change own password | Body `{current_password, new_password}` (min 10 chars). On success the session remains valid; other sessions are unaffected (password rotation revokes nothing — only explicit resets do). |

### Users (**admin**)

| Method & path | Purpose | Notes |
|---|---|---|
| `GET /api/v1/users?active=1` | List users | `active=1` hides deactivated accounts. |
| `POST /api/v1/users` | Create user | Body `{email, display_name?, role?}` (`role` defaults to `member`). Returns `{user, temporary_password}` — the temp password is shown **once** (unambiguous alphabet, e.g. `OFPD-K4XT-9M2Q`). `409 email_taken` on duplicates. |
| `PATCH /api/v1/users/{id}` | Update user | `{display_name?, role?, deactivated?}`. Demoting or deactivating the last active admin is rejected (`409 last_active_admin`). Deactivation revokes all of the user's sessions. |
| `POST /api/v1/users/{id}/password-reset` | Admin password reset | Returns a new `{temporary_password}` and revokes all sessions of the user. |
| `POST /api/v1/users/{id}/sessions/revoke` | Kill sessions | Returns `{revoked: n}`. |

### Photos

| Method & path | Purpose | Notes |
|---|---|---|
| `POST /api/v1/photos/upload` | Upload photographs | Multipart, field `files` (repeatable). Caps: `server.max_upload_files` files (default 50), `server.max_upload_bytes` each (default 50 MiB), `server.max_decode_dimension` px (default 8000). Accepts JPEG, PNG and **lossy** WebP (ADR-0009); HEIC and lossless/extended WebP fail per-file with `unsupported_media_type`. Duplicates (same sha256) link to the existing row (`duplicate: true`) without re-storing. Response `{created: [{photo, duplicate}], failed: [{name, error, code}]}` — `201` when at least one photo is created, `422` when none. |
| `GET /api/v1/photos?search=&album=&deleted=1&limit=&offset=` | List | `search` matches filename/caption (case-insensitive); `deleted=1` is the recovery view (scoped to own uploads unless admin); `limit` ≤ 500. |
| `GET /api/v1/photos/{id}` | One photo | Includes `albums` membership. |
| `PATCH /api/v1/photos/{id}` | Edit metadata | `{caption?, capture_date?}` (RFC3339). |
| `DELETE /api/v1/photos/{id}` | Soft delete | Members: own uploads only. Stored objects are kept (recovery + shared-album references). |
| `POST /api/v1/photos/{id}/restore` | Restore | Members: own uploads only. |
| `GET /api/v1/photos/{id}/file?variant=` | File bytes | `variant` = `original` (uploader/admin only — unstripped EXIF), `display` (1920 long edge), `thumb` (384²), `portrait` (1080×1620), `landscape` (1920×1200). Derivatives are re-encoded, EXIF-free JPEG. `private, max-age=3600`. |

### Albums

| Method & path | Purpose | Notes |
|---|---|---|
| `GET /api/v1/albums` | List | Shared albums for everyone; private albums for creator + admins only. |
| `POST /api/v1/albums` | Create | `{name, description?, shared?}` (default shared). |
| `GET /api/v1/albums/{id}` | Detail | `{album, photos: [photoID], devices: [deviceID]}`. |
| `PATCH /api/v1/albums/{id}` | Update | `{name?, description?, shared?, sort_order?}` (`sort_order` −1 = unchanged). Editable by everyone on shared albums; creator/admins on private. |
| `DELETE /api/v1/albums/{id}` | Soft delete | Creator/admins only. Photographs survive. |
| `POST /api/v1/albums/{id}/restore` | Restore | |
| `POST /api/v1/albums/{id}/photos` | Add photo | `{photo_id}`. Membership only — the stored original is never copied. First photo becomes the cover if none is set. |
| `DELETE /api/v1/albums/{id}/photos/{photoId}` | Remove photo | |
| `POST /api/v1/albums/{id}/cover` | Set cover | `{photo_id}` — must be a member of the album. |
| `POST /api/v1/albums/{id}/frames/{deviceId}` | Assign frame (**admin**) | Device registry from the `/devices` endpoints; the frame sync protocol consumes this in M3/M5. |
| `DELETE /api/v1/albums/{id}/frames/{deviceId}` | Unassign frame (**admin**) | |

### Devices (**admin**)

| Method & path | Purpose |
|---|---|
| `GET /api/v1/devices` | List (includes `album_count`, `revoked_at`, `has_credential`) |
| `POST /api/v1/devices` | Register by `{name}` |
| `PATCH /api/v1/devices/{id}` | Rename (`{name}`) |
| `POST /api/v1/devices/{id}/credential` | **M3** issue/rotate the device credential. Returns `{token, issued_at}` — the token is shown exactly once (only its SHA-256 is stored). Rotation invalidates the previous token immediately; revoked devices are rejected. |
| `DELETE /api/v1/devices/{id}/credential` | **M3** revoke the current credential (frame stops syncing). |
| `POST /api/v1/devices/{id}/revoke` | Revoke the whole device (all credentials). |
| `GET /api/v1/devices` | **M5** list now also returns per-device `status` (derived, see below) and a `health` block. |
| `GET /api/v1/devices/{id}` | **M5** device detail: registry row + latest `health` + current `config` document. |
| `GET /api/v1/devices/{id}/config` | **M5** read the per-frame config document (`{config, version}`). |
| `PUT /api/v1/devices/{id}/config` | **M5** replace the per-frame config (validated; `400` on bad input, never a partial write). Bumps `config_revision`, which flows to the manifest ETag. |
| `POST /api/v1/devices/{id}/sync` | **M5** trigger a sync. Bumps the device's `sync_request_seq`; the frame notices on its next ≤30 s heartbeat and checks the manifest immediately (no inbound connection). |

### Pairing (**admin** + frame-facing public)

The pairing protocol (M5) lets a frame register itself without anyone
typing a token. Three endpoints are **public** (a not-yet-paired frame
has no credential by definition); the admin endpoints require a session.

| Method & path | Purpose |
|---|---|
| `POST /api/v1/devices/pair-challenges` (frame, public) | Body `{device_id, device_name}`. Creates a pending challenge, returns `{challenge_id, code, expires_at, ttl_seconds}`. The 8-char code (31-symbol unambiguous alphabet, ~40 bits) is shown on the frame. Rate-limited per source IP (max 3 pending, 6 per 30 min). `409` if the device id is already registered. |
| `POST /api/v1/devices/pair-status` (frame, public) | Body `{challenge_id}`. Returns `{status: pending|approved|rejected|expired}`. When `approved` and not yet claimed, also returns `{device_id, credential}` — the credential is a **one-time claim**: the server nulls it after the first poll, so a replay gets nothing. |
| `GET /api/v1/pairing/pending` (**admin**) | Pending challenges (without the code): offered name, source IP, created/expires. |
| `POST /api/v1/pairing/{id}/approve` (**admin**) | Body `{code}`. Constant-time verify; on success atomically registers the device under its own identity and returns `{device, credential}` (shown once). `400` on a wrong code. |
| `POST /api/v1/pairing/{id}/reject` (**admin**) | Reject a pending challenge. |

### Health (device status derivation)

The frame posts a sanitized report via `POST /api/v1/frames/{deviceId}/health`
(device-authenticated). The server keeps only the **latest** report per
device and derives the `status` shown in the admin UI:

| Status | Meaning |
|---|---|
| `online` | heartbeat within the freshness window |
| `recently_seen` | seen, but outside the online window |
| `offline` | never seen or stale beyond the window |
| `sync_error` | latest report says the last sync failed (overlay on online/recently_seen) |
| `updating` / `update_failed` / `rollback` | M6 updater states — derived from `health.update_state`, override the heartbeat derivation |

The same response carries `sync_request_seq`, which is how the admin's
`POST /devices/{id}/sync` reaches the frame (see above).

### Audit & stats

| Method & path | Purpose | Notes |
|---|---|---|
| `GET /api/v1/audit?limit=` (**admin**) | Recent audit entries | New-first; every login, mutation and admin action is recorded (T-05/T-06). Detail is JSON and never contains credentials (T-13). |
| `GET /api/v1/stats` | Dashboard metrics | Photo/album/user/device counts, live storage bytes, last 10 uploads, server version/hostname. |

## Milestone 3 — frame synchronisation API

The frame API is **device-authenticated**, not session-authenticated:
`Authorization: Bearer <token>` where the token was issued by
`POST /api/v1/devices/{id}/credential`. The bearer token must belong to
the `{deviceId}` in the path — a token for one device cannot be used
against another device's resources. A 401 (code `bad_device_credential`)
is returned for missing, wrong, rotated or revoked credentials; the
response is identical for all of those so it does not distinguish them.

The frame initiates every request (spec §12); the server never connects
out to a frame.

| Method & path | Purpose |
|---|---|
| `GET /api/v1/frames/{deviceId}/manifest` | The device's synchronisation manifest (see below). Supports conditional requests: send `If-None-Match: <etag>` to get an empty `304` when nothing changed. |
| `GET /api/v1/frames/{deviceId}/photos/{resource}` | Download one derivative, where `{resource}` is `<hash>.display` or `<hash>.thumb`. Single-range requests (`Range: bytes=N-`) are honoured for resumable downloads. A photo is downloadable only if it is a live member of an album assigned to this device; anything else is a `404`. |
| `POST /api/v1/frames/{deviceId}/changes` | Idempotent upload of local frame changes. Body `{"ops":[{op_id, type, photo_hash, value, ts}]}`. `type` is `favourite` in M3. `op_id` deduplication makes replays after partial failures safe; conflicts resolve last-write-wins on `ts`. Response `{accepted, duplicates, rejected, rejected_ops}`. |

### Manifest shape

```
{
  "protocol": "openframe-manifest/1",
  "revision": 3,              // content-derived, monotonic per device
  "config_revision": 2,       // independent; bumps on device rename (M5 adds settings)
  "config": { "name": "Living room" },
  "albums": [
    {
      "id": "…", "name": "…", "cover_hash": "…", "priority": true,
      "photos": [
        {
          "hash": "<original sha256>", "name": "…", "caption": "…",
          "capture_date": "…", "priority": true, "favourite": true,
          "width": 1200, "height": 800,
          "display": { "resource": "<hash>.display", "size_bytes": 57201, "sha256": "…" },
          "thumb":   { "resource": "<hash>.thumb",   "size_bytes": 15570, "sha256": "…" }
        }
      ]
    }
  ]
}
```

- The **revision** advances only when the canonical manifest content
  actually changes, so an unchanged 60-minute check costs one round
  trip and zero bytes (304).
- `sha256`/`size_bytes` per file are the integrity contract: the frame
  verifies both before committing a file to its cache (spec §9 download
  safety). Derivative hashes are recorded at generation time in
  `photo_derivatives` and backfilled on first build for M2-era photos.
- Files reference **authenticated resource identifiers**, never
  permanent object-storage URLs (spec §9).
- A photo is `priority` when it or any of its albums carries the
  priority flag (spec §9 priority delivery).
- `favourite` reflects the device-scoped favourite state the frame
  uploaded (spec §5 per-frame favourites).

### Priority flags (**admin**)

| Method & path | Purpose |
|---|---|
| `POST /api/v1/photos/{id}/priority` | `{"priority": bool}` — mark a photograph for priority delivery. |
| `POST /api/v1/albums/{id}/priority` | `{"priority": bool}` — mark a collection; every photo in it is priority-delivered. |

## Milestone 5 — multi-frame management

Everything above (pairing, health, per-frame config, remote sync) plus
one manifest addition: the manifest gains a `config` block and a
`config_revision`. The ETag is `"<content_revision>:<config_revision>"`,
so a **config-only** change re-fetches the manifest (new ETag) without
any photo-download churn, and a config-only 304 is impossible while the
config is fresh. The `config` block carries the validated per-frame
document (name, slideshow interval/transition, sleep window,
brightness, orientation, captions, clock, sync interval). Precedence on
the frame is **local settings > server config > config file**.

## Milestone 6 — releases and update state

Public (unauthenticated) release endpoints, served by the management
server:

```
GET /api/v1/releases/latest.json?channel=&os=&arch=
        envelope: { release: <exact signed bytes>, serving: { rollout_percent, rollout_devices } }
GET /api/v1/releases/<version>/artifact?channel=&os=&arch=
        the release binary (Range requests supported)
```

The server stores the uploaded signed metadata **verbatim** and never
re-serialises it; the `serving` block is unsigned operator policy that
may change WHO receives a release, never WHAT (see
[docs/updater/trust-model.md](../updater/trust-model.md)).

Admin (session + CSRF, admin role):

```
POST   /api/v1/releases                    multipart: metadata (signed JSON) + artifact; lands as staged
GET    /api/v1/releases                    list (version, channel, platform, status, rollout, signer, expiry)
POST   /api/v1/releases/<version>/approve  staged → approved (becomes latest for its platform)
POST   /api/v1/releases/<version>/pause    approved → paused (latest moves to next approved)
PUT    /api/v1/releases/<version>/rollout  { percent, devices } — the serving override
DELETE /api/v1/releases/<version>          staged or paused only (approved must pause first)
```

Releases are keyed by (version, channel, os, arch). A newer approved
release replaces the latest pointer; pausing the latest re-exposes the
next approved one. Uploads are rejected when the signed metadata is
malformed, unsigned, or its artifact hash/size disagrees with the
document.

Update state in the device model:

- `health.update_state` — `none | updating | update_failed | rollback
  | unknown`, reported by the frame from the updater's state file.
- Device status derivation: `updating` / `update_failed` /
  `rollback` override the online/offline heartbeat derivation.
- `config.update_channel` — per-frame release channel (validated:
  `stable | beta | development`), delivered in the manifest and
  persisted by the frame in its own DB (the root updater reads it
  there).
