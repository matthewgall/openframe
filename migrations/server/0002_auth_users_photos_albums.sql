-- Server schema, milestone 2: authentication, users, photos, albums,
-- minimal devices, audit log.
--
-- All changes are additive (ADR-0008). Soft deletes use *_at columns so
-- recovery is a single UPDATE.

-- Users ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS users (
    id             TEXT PRIMARY KEY,          -- UUIDv4
    email          TEXT NOT NULL UNIQUE COLLATE NOCASE,
    display_name   TEXT NOT NULL,
    role           TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    password_hash  TEXT NOT NULL,             -- argon2id PHC string
    created_at     TEXT NOT NULL,             -- RFC3339 UTC
    updated_at     TEXT NOT NULL,
    deactivated_at TEXT                       -- set => cannot authenticate
);

CREATE INDEX IF NOT EXISTS idx_users_email ON users (email);

-- Sessions -------------------------------------------------------------
-- Tokens are 32 random bytes; only the SHA-256 of the token is stored.
-- The per-session CSRF token (synchronizer pattern) is derived from the
-- session token (HMAC-style SHA-256), so it is recomputable without
-- storing a second secret.

CREATE TABLE IF NOT EXISTS sessions (
    id             TEXT PRIMARY KEY,          -- UUIDv4
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash     TEXT NOT NULL UNIQUE,
    user_agent     TEXT NOT NULL DEFAULT '',
    ip             TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,
    revoked_at     TEXT
);

CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions (user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_token ON sessions (token_hash);

-- Photos ---------------------------------------------------------------
-- Content-addressed: hash is the SHA-256 of the original bytes. The
-- physical original lives in storage at originals/<hash>; a photo row
-- may share its hash with other rows (duplicate uploads) without
-- duplicating the stored object.

CREATE TABLE IF NOT EXISTS photos (
    id             TEXT PRIMARY KEY,          -- UUIDv4
    hash           TEXT NOT NULL,             -- sha256 hex of original
    original_name  TEXT NOT NULL,
    mime           TEXT NOT NULL,             -- image/jpeg | image/png | image/webp
    width          INTEGER NOT NULL,          -- stored (post-orientation) px
    height         INTEGER NOT NULL,
    size_bytes     INTEGER NOT NULL,
    capture_date   TEXT,                      -- RFC3339, from EXIF if present
    caption        TEXT NOT NULL DEFAULT '',
    uploaded_by    TEXT NOT NULL REFERENCES users(id),
    created_at     TEXT NOT NULL,
    deleted_at     TEXT                       -- soft delete
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_photos_hash ON photos (hash);
CREATE INDEX IF NOT EXISTS idx_photos_uploaded_by ON photos (uploaded_by);
CREATE INDEX IF NOT EXISTS idx_photos_deleted ON photos (deleted_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_photos_capture ON photos (capture_date);

-- Albums ---------------------------------------------------------------

CREATE TABLE IF NOT EXISTS albums (
    id             TEXT PRIMARY KEY,          -- UUIDv4
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    is_shared      INTEGER NOT NULL DEFAULT 1, -- 1: all members; 0: creator+admins
    cover_photo_id TEXT REFERENCES photos(id) ON DELETE SET NULL,
    sort_order     INTEGER NOT NULL DEFAULT 0,
    created_by     TEXT NOT NULL REFERENCES users(id),
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    deleted_at     TEXT                       -- soft delete
);

CREATE INDEX IF NOT EXISTS idx_albums_order ON albums (sort_order, created_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_albums_cover ON albums (cover_photo_id);

-- Album membership: a photo may belong to many albums; the original
-- stored object is never duplicated.

CREATE TABLE IF NOT EXISTS album_photos (
    album_id   TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    photo_id   TEXT NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
    sort_order INTEGER NOT NULL DEFAULT 0,
    added_at   TEXT NOT NULL,
    PRIMARY KEY (album_id, photo_id)
);

CREATE INDEX IF NOT EXISTS idx_album_photos_photo ON album_photos (photo_id);

-- Devices (minimal in M2 for album assignment; pairing, credentials and
-- health arrive in milestone 5) -----------------------------------------

CREATE TABLE IF NOT EXISTS devices (
    id         TEXT PRIMARY KEY,              -- UUIDv4
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    revoked_at TEXT
);

-- Album-to-frame assignment.

CREATE TABLE IF NOT EXISTS album_devices (
    album_id   TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    device_id  TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    assigned_at TEXT NOT NULL,
    PRIMARY KEY (album_id, device_id)
);

-- Audit log ------------------------------------------------------------
-- Administrative and security-relevant actions (T-05/T-06). Detail is a
-- JSON object; it must never contain passwords or tokens.

CREATE TABLE IF NOT EXISTS audit_log (
    id          INTEGER PRIMARY KEY,
    actor_user_id TEXT,                       -- NULL for system actions
    action      TEXT NOT NULL,                -- e.g. user.created, login.failed
    target_type TEXT NOT NULL DEFAULT '',
    target_id   TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '{}',   -- JSON
    ip          TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log (created_at);
CREATE INDEX IF NOT EXISTS idx_audit_target ON audit_log (target_type, target_id);
