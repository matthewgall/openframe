-- Server schema, milestone 1 baseline.
--
-- Real user/album/photo/device tables arrive with their milestones
-- (0002+ for M2 users+photos, etc.). This baseline creates the instance
-- metadata table used from day one.

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Server instance identity, set by Go bootstrap when absent.
CREATE INDEX IF NOT EXISTS idx_meta_key ON meta (key);
