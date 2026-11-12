-- Server schema, milestone 3: device credentials and frame
-- synchronisation state (spec §9, §12).
--
-- Additive only (ADR-0008): M2 code never reads these columns/tables.

-- Manifest revision counter per device. The manifest builder bumps it
-- only when the canonical manifest content actually changes, so frames
-- get a stable ETag for unchanged content (spec §9: "Use revision
-- identifiers and HTTP ETags").
ALTER TABLE devices ADD COLUMN manifest_revision INTEGER NOT NULL DEFAULT 0;

-- The device's last-built manifest content hash, so the revision
-- counter is content-derived (no spurious bumps, no missed changes).
ALTER TABLE devices ADD COLUMN manifest_hash TEXT NOT NULL DEFAULT '';

-- Per-frame configuration fingerprint (M3: the device name; M5 adds
-- per-frame settings). Its revision is independent of the manifest
-- content revision, per spec §9 ("Configuration revision" is a
-- manifest field).
ALTER TABLE devices ADD COLUMN config_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN config_hash TEXT NOT NULL DEFAULT '';

-- Device credentials. The raw token is shown to the operator exactly
-- once at issue time; only its SHA-256 is stored (T-04 pattern: never
-- persist a secret that does not need to be persisted). One credential
-- per device; issuing a new one rotates (invalidates) the old.
CREATE TABLE IF NOT EXISTS device_credentials (
    device_id   TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    issued_at   TEXT NOT NULL,
    revoked_at  TEXT
);

-- Priority delivery (spec §9: "Administrators may mark a photograph
-- or collection as priority"). A photo is priority-delivered if it or
-- any of its albums carries the flag.
ALTER TABLE photos ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
ALTER TABLE albums ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;

-- Device-scoped favourites (spec §5: per-frame favourite state, synced
-- from the frame). Updated by the changes-ingest endpoint with
-- last-write-wins on the frame's change timestamp.
CREATE TABLE IF NOT EXISTS favourites (
    device_id  TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    photo_hash TEXT NOT NULL,
    value      INTEGER NOT NULL CHECK (value IN (0, 1)),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (device_id, photo_hash)
);
CREATE INDEX IF NOT EXISTS idx_favourites_photo ON favourites(photo_hash);

-- Integrity hashes of generated derivatives (spec §9 "Download
-- safety" step 4: frames verify SHA-256 before committing a file).
-- The manifest serves these hashes; the frame rejects any file whose
-- bytes do not match. Recorded at generation time; backfilled on
-- first manifest build for photos ingested before this release.
CREATE TABLE IF NOT EXISTS photo_derivatives (
    photo_hash   TEXT NOT NULL,
    variant      TEXT NOT NULL,
    sha256       TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL,
    generated_at TEXT NOT NULL,
    PRIMARY KEY (photo_hash, variant)
);

-- Idempotency ledger for frame change operations: a (device, op_id)
-- pair is applied at most once even if the frame replays it after a
-- partial failure or network timeout (spec §9: "Use idempotent
-- operations and explicit conflict-resolution rules").
CREATE TABLE IF NOT EXISTS device_ops (
    device_id  TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    op_id      TEXT NOT NULL,
    applied_at TEXT NOT NULL,
    PRIMARY KEY (device_id, op_id)
);
