-- Frame schema, milestone 3: offline-first synchronisation (spec §8).
--
-- All tables are additive (ADR-0008): binary rollback to M1 is safe
-- because M1 code never reads these tables. The frame must survive an
-- indefinite loss of the server; every table below is fully local and
-- nothing here requires a round-trip to the server to be usable.

-- Albums of the ACTIVE collection (what the renderer shows). Written
-- atomically at activation, mirroring photos.
CREATE TABLE IF NOT EXISTS albums (
    id                TEXT PRIMARY KEY,  -- server album id
    name              TEXT NOT NULL,
    cover_hash        TEXT,              -- content hash of cover photo (nullable)
    fetched_revision  INTEGER NOT NULL   -- manifest revision this row came from
);

-- Albums of the STAGING collection, merged into albums at activation.
CREATE TABLE IF NOT EXISTS staging_albums (
    collection_revision INTEGER NOT NULL REFERENCES collections(revision) ON DELETE CASCADE,
    id                  TEXT NOT NULL,
    name                TEXT NOT NULL,
    cover_hash          TEXT,
    PRIMARY KEY (collection_revision, id)
);

-- The ACTIVE collection: exactly what the renderer displays. This
-- table is only rewritten atomically at activation (spec §8: keep the
-- previous working collection available until the new one is safely
-- activated). display_path/thumb_path are always set here because a
-- photo only enters the table once its files are verified in the
-- cache. Keyed by content hash (content-addressed; identical originals
-- dedupe naturally).
CREATE TABLE IF NOT EXISTS photos (
    hash          TEXT PRIMARY KEY,
    album_id      TEXT NOT NULL,
    name          TEXT NOT NULL,
    caption       TEXT NOT NULL DEFAULT '',
    capture_date  TEXT,                  -- RFC3339 or NULL
    display_bytes INTEGER NOT NULL DEFAULT 0,
    thumb_bytes   INTEGER NOT NULL DEFAULT 0,
    display_path  TEXT NOT NULL,
    thumb_path    TEXT,
    priority      INTEGER NOT NULL DEFAULT 0,
    is_favourite  INTEGER NOT NULL DEFAULT 0,
    downloaded_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_photos_album ON photos(album_id);
CREATE INDEX IF NOT EXISTS idx_photos_capture ON photos(capture_date);

-- The STAGING collection: the incoming manifest, held until every
-- mandatory file is verified, then merged into photos atomically.
-- Keyed by (collection_revision, hash). The download queue references
-- the same files; a queue row is removed when its file is committed to
-- the cache.
CREATE TABLE IF NOT EXISTS staging_photos (
    collection_revision INTEGER NOT NULL REFERENCES collections(revision) ON DELETE CASCADE,
    hash                TEXT NOT NULL,
    album_id            TEXT NOT NULL,
    name                TEXT NOT NULL,
    caption             TEXT NOT NULL DEFAULT '',
    capture_date        TEXT,
    display_bytes       INTEGER NOT NULL DEFAULT 0,
    thumb_bytes         INTEGER NOT NULL DEFAULT 0,
    display_sha         TEXT,            -- integrity hash of the display file
    thumb_sha           TEXT,
    display_resource    TEXT,
    thumb_resource      TEXT,
    priority            INTEGER NOT NULL DEFAULT 0,
    is_favourite        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (collection_revision, hash)
);
CREATE INDEX IF NOT EXISTS idx_staging_hash ON staging_photos(hash);

-- Collections are the unit of atomic activation (spec §8: "A new
-- manifest must not become active until all mandatory files have been
-- verified. Keep the previous working collection available until the
-- new collection is safely activated.").
--
-- states: 'staging' (downloading/verifying), 'active' (what the
-- renderer displays), 'retired' (previous active, eligible for GC).
-- There is at most one 'active' and one 'staging' collection at a
-- time; the sync engine enforces this.
CREATE TABLE IF NOT EXISTS collections (
    revision      INTEGER PRIMARY KEY,   -- manifest revision
    photo_count   INTEGER NOT NULL,
    total_bytes   INTEGER NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('staging', 'active', 'retired')),
    created_at    TEXT NOT NULL,
    activated_at  TEXT
);

-- Download queue: one row per (collection, photo, variant) file that
-- still has to be fetched. 'downloading' rows are re-adopted by a
-- restarted frame (crash-safe resume); 'failed' rows retry with
-- exponential backoff recorded in next_attempt.
CREATE TABLE IF NOT EXISTS download_queue (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    collection_revision   INTEGER NOT NULL REFERENCES collections(revision) ON DELETE CASCADE,
    hash                  TEXT NOT NULL,  -- original content hash (cache key prefix)
    variant               TEXT NOT NULL CHECK (variant IN ('display', 'thumb')),
    bytes                 INTEGER NOT NULL DEFAULT 0,
    sha256                TEXT NOT NULL,  -- integrity hash of the derivative (manifest)
    resource              TEXT NOT NULL,  -- authenticated resource path (manifest)
    state                 TEXT NOT NULL CHECK (state IN ('pending', 'downloading', 'failed')),
    attempts              INTEGER NOT NULL DEFAULT 0,
    next_attempt          TEXT,          -- RFC3339 UTC; NULL = due now
    last_error            TEXT,
    UNIQUE (collection_revision, hash, variant)
);
CREATE INDEX IF NOT EXISTS idx_dq_due ON download_queue(state, next_attempt);

-- Local favourites state, usable offline (spec §5: "Favourites must
-- remain usable offline", per-frame state).
CREATE TABLE IF NOT EXISTS favourites (
    photo_hash  TEXT PRIMARY KEY,
    value       INTEGER NOT NULL CHECK (value IN (0, 1)),
    updated_at  TEXT NOT NULL
);

-- Offline change queue: idempotent operations to replay against the
-- server when connectivity returns. op_id is a stable UUID generated
-- when the change is made; the server dedupes on (device, op_id) so
-- replays after partial failures are safe.
CREATE TABLE IF NOT EXISTS favourites_queue (
    op_id       TEXT PRIMARY KEY,
    photo_hash  TEXT NOT NULL,
    value       INTEGER NOT NULL,
    ts          TEXT NOT NULL,           -- when the change happened on the frame
    created_at  TEXT NOT NULL
);

-- Synchronisation bookkeeping (same shape as agent_state but kept
-- separate so M5 health reporting can distinguish app state from sync
-- state). Keys: last_attempt, last_success, last_error,
-- consecutive_failures, last_manifest_revision, last_config_revision.
CREATE TABLE IF NOT EXISTS sync_state (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
