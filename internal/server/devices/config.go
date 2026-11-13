package devices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/updater"
)

// FrameConfig is the administrator-managed configuration pushed to a
// frame through the manifest (spec §6). Zero/empty fields mean "not
// set" and fall through to the frame's local settings, then to its
// config file defaults. The precedence, highest first, is:
//
//	frame UI local settings  >  server frame config  >  config file
type FrameConfig struct {
	SlideshowIntervalSec int    `json:"slideshow_interval_sec"`
	Transition           string `json:"transition"` // none|crossfade|slide
	TransitionDurationMS int    `json:"transition_duration_ms"`
	SyncIntervalSec      int    `json:"sync_interval_sec"`
	SleepOn              string `json:"sleep_on"`   // "HH:MM" 24h or ""
	SleepOff             string `json:"sleep_off"`  // "HH:MM" 24h or ""
	Brightness           *int   `json:"brightness"` // 0..100
	Orientation          string `json:"orientation"`
	ShowClock            *bool  `json:"show_clock"`
	ShowCaptions         *bool  `json:"show_captions"`
	// UpdateChannel is the release channel this frame updates from
	// (M6, spec §14): stable | beta | development. Stored in the
	// frame DB by the frame and read by the root updater; an empty
	// value means "not set" (the updater defaults to stable).
	UpdateChannel string `json:"update_channel"`
}

var validTransitions = map[string]bool{"none": true, "crossfade": true, "slide": true}

// ConfigValidationError indicates a config document that failed
// validation (mapped to HTTP 400 by the API layer).
type ConfigValidationError struct{ Msg string }

func (e *ConfigValidationError) Error() string { return e.Msg }

// Validate bounds every field; invalid documents are rejected on
// write so a frame never receives a config it cannot apply.
func (c *FrameConfig) Validate() error {
	if c.SlideshowIntervalSec != 0 && (c.SlideshowIntervalSec < 2 || c.SlideshowIntervalSec > 3600) {
		return errors.New("config: slideshow_interval_sec must be 2..3600 or 0")
	}
	if c.Transition != "" && !validTransitions[c.Transition] {
		return fmt.Errorf("config: invalid transition %q", c.Transition)
	}
	if c.TransitionDurationMS != 0 && (c.TransitionDurationMS < 50 || c.TransitionDurationMS > 10000) {
		return errors.New("config: transition_duration_ms must be 50..10000 or 0")
	}
	if c.SyncIntervalSec != 0 && (c.SyncIntervalSec < 10 || c.SyncIntervalSec > 86400) {
		return errors.New("config: sync_interval_sec must be 10..86400 or 0")
	}
	for _, v := range []struct {
		name, val string
	}{
		{"sleep_on", c.SleepOn},
		{"sleep_off", c.SleepOff},
	} {
		if v.val == "" {
			continue
		}
		if len(v.val) != 5 {
			return fmt.Errorf("config: %s must be HH:MM (24h, zero-padded)", v.name)
		}
		if _, err := time.Parse("15:04", v.val); err != nil {
			return fmt.Errorf("config: %s must be HH:MM (24h)", v.name)
		}
	}
	if c.Brightness != nil && (*c.Brightness < 0 || *c.Brightness > 100) {
		return errors.New("config: brightness must be 0..100")
	}
	if c.Orientation != "" && !strings.EqualFold(c.Orientation, "normal") &&
		!strings.EqualFold(c.Orientation, "rotate90") &&
		!strings.EqualFold(c.Orientation, "rotate180") &&
		!strings.EqualFold(c.Orientation, "rotate270") {
		return fmt.Errorf("config: invalid orientation %q", c.Orientation)
	}
	if c.UpdateChannel != "" && !updater.ValidChannel(c.UpdateChannel) {
		return fmt.Errorf("config: invalid update_channel %q (want stable, beta or development)", c.UpdateChannel)
	}
	return nil
}

// ConfigWithVersion is the stored document plus its change counter.
type ConfigWithVersion struct {
	Config    FrameConfig `json:"config"`
	Version   int64       `json:"version"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// GetFrameConfig returns the stored config (zero-value + version 0
// when none has been set).
func (s *Store) GetFrameConfig(ctx context.Context, deviceID string) (*ConfigWithVersion, error) {
	var (
		doc     string
		version int64
		updated string
	)
	err := s.dbh.QueryRowContext(ctx,
		`SELECT config, version, updated_at FROM frame_configs WHERE device_id = ?`,
		deviceID).Scan(&doc, &version, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return &ConfigWithVersion{Config: FrameConfig{}, Version: 0}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg FrameConfig
	if doc != "" {
		if err := json.Unmarshal([]byte(doc), &cfg); err != nil {
			return nil, fmt.Errorf("config: corrupt stored document: %w", err)
		}
	}
	ut, _ := time.Parse(time.RFC3339, updated)
	return &ConfigWithVersion{Config: cfg, Version: version, UpdatedAt: ut}, nil
}

// SetFrameConfig validates and stores a new config, bumping the
// version so frames can detect the change.
func (s *Store) SetFrameConfig(ctx context.Context, deviceID string, cfg FrameConfig) (*ConfigWithVersion, error) {
	if err := cfg.Validate(); err != nil {
		return nil, &ConfigValidationError{Msg: err.Error()}
	}
	var exists int
	err := s.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM devices WHERE id = ?", deviceID).Scan(&exists)
	if err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrNotFound
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.dbh.ExecContext(ctx,
		`INSERT INTO frame_configs (device_id, config, version, updated_at)
		 VALUES (?, ?, 1, ?)
		 ON CONFLICT(device_id) DO UPDATE SET
		   config = excluded.config,
		   version = version + 1,
		   updated_at = excluded.updated_at`,
		deviceID, string(doc), now)
	if err != nil {
		return nil, err
	}
	return s.GetFrameConfig(ctx, deviceID)
}
