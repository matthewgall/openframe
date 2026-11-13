-- 0005_releases: release hosting for the secure updater (M6, spec §14,
-- ADR-0007).
--
-- The server stores the EXACT signed metadata document uploaded by the
-- release tooling (metadata column, canonical bytes). It never
-- re-serialises or mutates it: latest.json serves the stored bytes
-- verbatim so the frame's signature check covers exactly what was
-- signed.
--
-- Serving model: a release is visible to frames only when
-- status = 'approved'. At most one approved release is the "latest"
-- per (channel, os, arch) — the explicit is_latest pointer (set on
-- approve, recomputed on pause/delete) is what frames fetch. This
-- makes rollouts deterministic and operator-controlled: approving a
-- signed rollback release flips the pointer to it.

CREATE TABLE releases (
    version              TEXT    NOT NULL,
    channel              TEXT    NOT NULL,
    os                   TEXT    NOT NULL,
    arch                 TEXT    NOT NULL,
    artifact_sha256      TEXT    NOT NULL,
    artifact_size        INTEGER NOT NULL,
    -- The complete signed Release JSON exactly as uploaded.
    metadata             TEXT    NOT NULL,
    -- staged (uploaded, not served) | approved (served) | paused
    -- (suspended, e.g. a bad build discovered post-rollout).
    status               TEXT    NOT NULL DEFAULT 'staged'
        CHECK (status IN ('staged', 'approved', 'paused')),
    -- Serving pointer: 1 when this release is the one latest.json
    -- returns for its (channel, os, arch). At most one per triple.
    is_latest            INTEGER NOT NULL DEFAULT 0,
    -- Staged-rollout fields (part of the signed metadata; mirrored
    -- here so the admin UI can show/audit them and so the server can
    -- enforce that an approved release is actually rolled out to
    -- 100% before it is treated as "current" for stats).
    rollout_percent      INTEGER NOT NULL DEFAULT 100
        CHECK (rollout_percent BETWEEN 0 AND 100),
    rollout_devices      TEXT    NOT NULL DEFAULT '[]',
    released_at          TEXT    NOT NULL,
    not_before           TEXT    NOT NULL DEFAULT '',
    expires_at           TEXT    NOT NULL,
    min_protocol_version INTEGER NOT NULL DEFAULT 1,
    min_schema_version   INTEGER NOT NULL DEFAULT 1,
    is_rollback          INTEGER NOT NULL DEFAULT 0,
    signer_key_id        TEXT    NOT NULL DEFAULT '',
    created_at           TEXT    NOT NULL,
    updated_at           TEXT    NOT NULL,
    PRIMARY KEY (version, channel, os, arch)
);

CREATE INDEX idx_releases_serving ON releases (channel, os, arch, is_latest);
