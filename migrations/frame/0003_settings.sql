-- Frame schema, milestone 4: local settings (spec §10 "Settings mode").
--
-- Additive (ADR-0008): M3 binaries ignore this table, so rollback to
-- M3 is safe.
--
-- Values written by the frame's own settings screen (and remote
-- overrides when the M5 per-frame configuration lands) as JSON-ish
-- text per key. They OVERRIDE the config file for the duration of the
-- frame's life; wiping the data directory resets them.
--
-- Keys:
--   brightness          "0.8"            (0.0-1.0)
--   slideshow_interval  "45s"            (Go duration)
--   transition          "crossfade"      (none|crossfade|slide)
--   orientation         "90"             (0|90|180|270)
--   sleep_on            "07:00"
--   sleep_off           "22:00"
--   show_caption        "true"
--   show_clock          "true"
CREATE TABLE IF NOT EXISTS frame_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
