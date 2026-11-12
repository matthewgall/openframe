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

### Devices (**admin**, M2 minimal registry)

Pairing, credentials, health and sync controls arrive in M5; M2 provides
the registry that album assignment references.

| Method & path | Purpose |
|---|---|
| `GET /api/v1/devices` | List (includes `album_count`, `revoked_at`) |
| `POST /api/v1/devices` | Register by `{name}` |
| `PATCH /api/v1/devices/{id}` | Rename (`{name}`) |
| `POST /api/v1/devices/{id}/revoke` | Mark credentials revoked |

### Audit & stats

| Method & path | Purpose | Notes |
|---|---|---|
| `GET /api/v1/audit?limit=` (**admin**) | Recent audit entries | New-first; every login, mutation and admin action is recorded (T-05/T-06). Detail is JSON and never contains credentials (T-13). |
| `GET /api/v1/stats` | Dashboard metrics | Photo/album/user/device counts, live storage bytes, last 10 uploads, server version/hostname. |

## Planned endpoint map (M3–M6)

```
/api/v1/frames/{id}/manifest        conditional manifest (M3)
/api/v1/frames/{id}/photos/{hash}   authenticated download (M3)
/api/v1/frames/{id}/changes         offline change upload (M3)
/api/v1/frames/{id}/health          health report ingest (M5)
/api/v1/pairing/...                 challenge + approval (M5)
/api/v1/releases/...                release metadata (M6)
```

Directional; exact shapes are specified in each milestone.
