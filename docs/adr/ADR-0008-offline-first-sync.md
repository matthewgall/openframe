# ADR-0008: Offline-first sync — content-addressed cache, staged activation, additive migrations

**Status:** accepted (milestone 1); implemented in M3 (see Consequences for what shipped)

## Context

The frame must keep displaying family photos through any outage,
including prolonged ones. Naïve "download into place" designs corrupt
the active collection on interrupted downloads, and naive garbage
collection deletes photos still on screen. Binary rollback (M6) is
also unsafe if a new binary cannot read the old database.

## Decision

**Content-addressed cache.** Photographs are stored by SHA-256.
Metadata (albums, captions, ordering) references hashes, never paths.
Downloads write to a temp file, verify the hash, then rename into the
cache — a hash-named file is either complete-and-valid or absent.

**Staged activation.** The "collection" (albums + photo set + config)
is a revisioned object in local SQLite. A new manifest becomes active
only after *all* mandatory files are present and verified. The previous
working collection remains intact until then, and GC never removes
files referenced by the active collection (refcount by collection
revision).

**Protocol.** Conditional manifest GETs (ETag/304), authenticated
resource references (no permanent storage URLs), resumable downloads
where the backend permits, priority delivery that bypasses schedule but
never verification.

**Database compatibility.** Migrations are additive and
backward-compatible (see architecture doc). Incompatible changes require
a verified-backup-and-restore path; the updater rejects updates where
safe rollback cannot be guaranteed.

## Consequences

- Interrupted download / server death / disk-full all degrade to
  "keep the last good collection" — no corruption path.
- GC is safe by construction (refcounted), not by timing.
- Cost: extra bookkeeping tables (revisions, refs, queue) and a
  slightly larger frame DB; negligible at family-photo scale.

## Implementation notes (M3)

- **Cache.** `internal/frame/cache` implements the content-addressed
  store under `DataDir/frame/photos` as `<hash>.<variant>`. Downloads
  write to `<hash>.<variant>.part`, verify SHA-256 and size, then
  rename into place. A hash-named file is therefore either
  complete-and-valid or absent. A configurable quota plus a free-space
  guard makes a full disk stop new downloads instead of corrupting
  state.
- **Staging vs active.** The frame DB keeps two namespaces: `albums`/
  `photos` (the active, displayable collection) and `staging_albums`/
  `staging_photos` (the in-flight manifest). A manifest is staged into
  the staging tables; the download queue is built from it; and
  **activation is a single transaction** that moves staging → active
  only when every required file is committed. On any failure the
  previous collection is untouched.
- **Refcounted GC.** `collections` tracks `active` and `retired`
  revisions. `cache.GC` never deletes a file still referenced by the
  active collection or by the most recent retired one (so a rollback
  of activation is possible), keeping disk bounded.
- **Protocol.** `internal/server/manifest` builds the manifest with a
  content-derived, monotonic `revision` and an independent
  `config_revision`. The handler serves an `ETag` and answers
  `If-None-Match` with an empty `304`, so an unchanged 60-minute poll
  transfers no payload. `photos` and `albums` carry `priority` flags
  and the manifest marks them for priority delivery.
- **Offline changes.** Favourites are the first frame-originated
  change. They are queued in `favourites_queue` (keyed by stable
  `op_id`), applied locally, and replayed to the server's idempotent
  `POST /changes` when connectivity returns; `op_id` dedup +
  last-write-wins on `ts` makes replays safe.
