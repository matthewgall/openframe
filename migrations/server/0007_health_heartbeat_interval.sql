-- 0007: the frame reports the interval at which it sends health
-- heartbeats (0 = unknown → server falls back to its build-time
-- default). Liveness thresholds become per-device instead of
-- assuming every frame pings every 30s (DeriveStatus/onlineWindow).
ALTER TABLE device_health
    ADD COLUMN heartbeat_interval INTEGER NOT NULL DEFAULT 0;
