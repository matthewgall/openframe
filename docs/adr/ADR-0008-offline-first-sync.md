# ADR-0008: Offline-first sync — content-addressed cache, staged activation, additive migrations

**Status:** accepted (milestone 1); implementation in M3

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
