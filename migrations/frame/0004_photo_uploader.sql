-- Photo attribution for the arrival debut (living-presentation R12):
-- the uploader's display name (manifest "uploader") and the
-- server-side receipt time (manifest "uploaded_at"), carried on both
-- the staging and active photo tables so the activation swap copies
-- them like every other column. Debut ordering (R7) sorts by
-- uploaded_at; empty values degrade to nameless debuts.

ALTER TABLE photos ADD COLUMN uploader TEXT NOT NULL DEFAULT '';
ALTER TABLE photos ADD COLUMN uploaded_at TEXT NOT NULL DEFAULT '';
ALTER TABLE staging_photos ADD COLUMN uploader TEXT NOT NULL DEFAULT '';
ALTER TABLE staging_photos ADD COLUMN uploaded_at TEXT NOT NULL DEFAULT '';
