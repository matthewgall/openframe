-- 0004 — Milestone 5: multi-frame management (spec §6, §12, §13).

-- Pairing challenges (spec §12). A frame requests a challenge, displays
-- the returned code, and an administrator enters that code in the admin
-- UI to approve. Codes are short-lived (server-side expiry) and
-- single-use (the challenge is consumed atomically with device
-- creation + credential issue). Only the SHA-256 hash of the code is
-- stored; approval compares in constant time.
CREATE TABLE IF NOT EXISTS pairing_challenges (
    id           TEXT PRIMARY KEY,              -- UUIDv4
    device_id    TEXT NOT NULL,                 -- the frame's own identity (UUID)
    device_name  TEXT NOT NULL,
    code_hash    TEXT NOT NULL,                 -- SHA-256 hex of the pairing code
    source_ip    TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending', -- pending|approved|rejected|expired
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    consumed_at  TEXT,
    approved_device_id TEXT REFERENCES devices(id) ON DELETE SET NULL,
    -- The raw credential, transient: written at approval so the frame
    -- can claim it on its next status poll, then nulled (one-time
    -- claim). device_credentials itself never stores raw tokens.
    credential   TEXT
);
CREATE INDEX IF NOT EXISTS idx_pairing_challenges_status
    ON pairing_challenges(status, created_at);
CREATE INDEX IF NOT EXISTS idx_pairing_challenges_ip
    ON pairing_challenges(source_ip, created_at);

-- Health reports (spec §13): the latest report per device. Status is
-- derived (never stored): online / recently seen / offline / sync error
-- / update states. Summaries only — no PII, no tokens, no paths.
CREATE TABLE IF NOT EXISTS device_health (
    device_id         TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    version           TEXT NOT NULL DEFAULT '',
    uptime_seconds    INTEGER NOT NULL DEFAULT 0,
    last_sync_at      TEXT NOT NULL DEFAULT '',
    last_sync_ok      INTEGER NOT NULL DEFAULT 0,
    manifest_revision INTEGER NOT NULL DEFAULT 0,
    pending_downloads INTEGER NOT NULL DEFAULT 0,
    storage_total     INTEGER NOT NULL DEFAULT 0,
    storage_used      INTEGER NOT NULL DEFAULT 0,
    cache_bytes       INTEGER NOT NULL DEFAULT 0,
    mem_total_mb      INTEGER NOT NULL DEFAULT 0,
    mem_used_mb       INTEGER NOT NULL DEFAULT 0,
    cpu_temp_c        REAL,
    display_driver    TEXT NOT NULL DEFAULT '',
    display_status    TEXT NOT NULL DEFAULT '',
    error_count_24h   INTEGER NOT NULL DEFAULT 0,
    recent_errors     TEXT NOT NULL DEFAULT '[]', -- JSON array of short summaries
    update_state      TEXT NOT NULL DEFAULT 'unknown',
    reported_at       TEXT NOT NULL
);

-- Per-frame configuration (spec §6): admin-managed settings distributed
-- through the manifest. The JSON document is validated server-side on
-- write; version increments on every change so frames can detect
-- updates.
CREATE TABLE IF NOT EXISTS frame_configs (
    device_id  TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    config     TEXT NOT NULL DEFAULT '{}',
    version    INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);

-- Remote sync requests (spec §6 "Trigger synchronisation") and
-- heartbeat freshness. The frame polls the server; the server never
-- initiates connections to frames (spec §12).
ALTER TABLE devices ADD COLUMN sync_request_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN last_heartbeat_at TEXT;
