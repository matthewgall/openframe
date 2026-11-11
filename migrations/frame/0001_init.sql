-- Frame schema, milestone 1 baseline.
--
-- Milestone 3 adds the synchronisation tables (manifests, download queue,
-- collections, favourites queue); milestone 4 adds display state. All
-- migrations are additive to keep binary rollback safe (ADR-0008).

-- Stable device identity. Exactly one row after first boot.
CREATE TABLE IF NOT EXISTS identity (
    id         TEXT PRIMARY KEY, -- UUIDv4
    created_at TEXT NOT NULL     -- RFC3339 UTC
);

-- Key/value state written by the frame agent. Values are JSON or plain
-- scalars; readers must tolerate missing keys (offline-first: any of
-- these may be absent until the first successful event).
CREATE TABLE IF NOT EXISTS agent_state (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
