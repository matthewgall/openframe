package frame

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/matthewgall/openframe/internal/config"
)

// Local settings (M4 settings screen) override the config file for
// this frame. They live in frame.db (frame_settings) and are applied
// at startup and after a hold-to-apply save from the settings screen
// (spec §10: "Administrative actions must be protected against
// accidental activation").

const (
	settingBrightness        = "brightness"
	settingSlideshowInterval = "slideshow_interval"
	settingTransition        = "transition"
	settingOrientation       = "orientation"
	settingSleepOn           = "sleep_on"
	settingSleepOff          = "sleep_off"
	settingShowCaption       = "show_caption"
	settingShowClock         = "show_clock"
)

// LoadLocalSettings reads the persisted overrides. All fields are
// pointers: nil means "use the config file value".
func LoadLocalSettings(ctx context.Context, dbh *sql.DB) map[string]string {
	out := map[string]string{}
	rows, err := dbh.QueryContext(ctx, "SELECT key, value FROM frame_settings")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

// SaveLocalSettings persists one key/value override.
func SaveLocalSetting(ctx context.Context, dbh *sql.DB, key, value string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := dbh.ExecContext(ctx,
		`INSERT INTO frame_settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, now)
	return err
}

// applyLocalSettings overlays persisted settings onto a config. It is
// a no-op for keys the frame has never changed.
func applyLocalSettings(c *config.FrameConfig, s map[string]string) error {
	if v, ok := s[settingBrightness]; ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return fmt.Errorf("local setting %s: %q invalid", settingBrightness, v)
		}
		c.Display.Brightness = f
	}
	if v, ok := s[settingSlideshowInterval]; ok {
		d, err := time.ParseDuration(v)
		if err != nil || d < 5*time.Second {
			return fmt.Errorf("local setting %s: %q invalid", settingSlideshowInterval, v)
		}
		c.Slideshow.Interval = d
	}
	if v, ok := s[settingTransition]; ok {
		if v != "none" && v != "crossfade" && v != "slide" {
			return fmt.Errorf("local setting %s: %q invalid", settingTransition, v)
		}
		c.Slideshow.Transition = v
	}
	if v, ok := s[settingOrientation]; ok {
		if v != "0" && v != "90" && v != "180" && v != "270" {
			return fmt.Errorf("local setting %s: %q invalid", settingOrientation, v)
		}
		c.Display.Orientation = v
	}
	if v, ok := s[settingSleepOn]; ok {
		c.Sleep.On = v
	}
	if v, ok := s[settingSleepOff]; ok {
		c.Sleep.Off = v
	}
	if v, ok := s[settingShowCaption]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("local setting %s: %q invalid", settingShowCaption, v)
		}
		c.Slideshow.ShowCaption = b
	}
	if v, ok := s[settingShowClock]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("local setting %s: %q invalid", settingShowClock, v)
		}
		c.Slideshow.ShowClock = b
	}
	return nil
}
