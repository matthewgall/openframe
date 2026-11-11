# REST API — Conventions and Milestone 1 Surface

OpenFrame exposes a versioned REST API under `/api/v1`. This document
fixes the conventions now so M2+ endpoints land without rework; the
only live endpoint in milestone 1 is the health probe.

## Live in milestone 1

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

## Conventions (M2+)

- **Versioning**: path-based (`/api/v1`). Breaking changes bump the
  major version; the old version stays available for at least one
  release cycle. Frames carry `MaxProtocolVersion`/
  `MinProtocolVersion` (see `internal/protocol`) and must ignore
  responses they cannot parse rather than crash.
- **Auth**: sessions are HTTP-only cookies with CSRF tokens for browser
  clients; frames authenticate with per-device credentials over HTTPS
  (M5 pairing). No bearer tokens in URLs, ever.
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

## Planned endpoint map (M2–M6)

```
/api/v1/auth/...       login, logout, session, password recovery (M2)
/api/v1/users/...      family accounts, roles, invitations (M2)
/api/v1/photos/...     upload, metadata, captions, delete/restore (M2)
/api/v1/albums/...     albums, covers, ordering (M2)
/api/v1/devices/...    frames, assignments, config, sync triggers (M5)
/api/v1/frames/{id}/manifest        conditional manifest (M3)
/api/v1/frames/{id}/photos/{hash}   authenticated download (M3)
/api/v1/frames/{id}/changes         offline change upload (M3)
/api/v1/frames/{id}/health          health report ingest (M5)
/api/v1/pairing/...                 challenge + approval (M5)
/api/v1/releases/...                release metadata (M6)
```

These are directional; exact shapes are specified in each milestone.
