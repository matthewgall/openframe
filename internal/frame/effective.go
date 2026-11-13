package frame

import (
	"context"
	"fmt"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/frame/sleep"
	"github.com/matthewgall/openframe/internal/frame/sync"
	"github.com/matthewgall/openframe/internal/frame/ui"
	"github.com/matthewgall/openframe/internal/updater"
)

// Effective configuration precedence (M5, spec §6), highest first:
//
//	1. local settings — changed on the frame (frame.db frame_settings)
//	2. server config — administered in the admin UI (per-device)
//	3. config file   — deployment defaults (YAML/env)
//
// recomputeEffective() layers them onto a pristine copy of the config
// file values (f.baseCfg), so a server config change can never be
// clobbered by stale in-memory values and a local override always wins.

// applyServerConfig is the sync-engine OnConfig callback (M5): the
// server's per-frame configuration advanced. It recomputes the
// effective config and applies it live (UI, sleep, sync interval,
// driver). Runs on the manifest-check goroutine; must be fast.
func (f *Frame) applyServerConfig(cfg sync.Config) {
	f.effMu.Lock()
	f.serverCfg = cfg
	f.hasServerCfg = true
	f.effMu.Unlock()

	if err := f.recomputeEffective(context.Background()); err != nil {
		f.log.Warn("apply server config failed", "error", err)
	}
	// Remember the document for offline display (doctor/status).
	setState(context.Background(), f.db, StateKeyServerConfig, formatServerConfig(cfg))
}

// recomputeEffective rebuilds f.cfg from base + server + local and
// applies it to the live UI/driver/sleep state. Called at startup and
// on every server-config change. The pointer swap is serialized with
// every runtime reader through effMu; the live apply happens after
// the lock is released (the UI callback takes the UI mutex — keeping
// the two locks non-nested).
func (f *Frame) recomputeEffective(ctx context.Context) error {
	f.effMu.Lock()
	cfg := f.baseCfg // value copy: config.Config is all value types
	if f.hasServerCfg {
		overlayServerConfig(&cfg.Frame, f.serverCfg)
	}
	local := LoadLocalSettings(ctx, f.db)
	hasServerCfg := f.hasServerCfg
	syncInterval := f.serverCfg.SyncIntervalSec
	err := applyLocalSettings(&cfg.Frame, local)
	if err == nil {
		f.cfg = &cfg
	}
	f.effMu.Unlock()
	if err != nil {
		return fmt.Errorf("local settings: %w", err)
	}

	// Publish the effective update channel (M6, spec §14) for the
	// root updater, which reads the frame DB directly. Resolved here
	// so the precedence (local > server > file) is already applied.
	channel := cfg.Frame.Update.Channel
	if channel == "" {
		channel = updater.ChannelStable
	}
	if err := setState(ctx, f.db, StateKeyUpdateChannel, channel); err != nil {
		f.log.Warn("update channel state write failed", "error", err)
	}

	// Live-apply to the running pieces (outside effMu).
	if f.ui != nil {
		f.applyEffectiveToUI(ctx)
	}
	if f.engine != nil && hasServerCfg {
		f.engine.SetManifestInterval(syncInterval)
	}
	return nil
}

// effectiveConfig returns a consistent value copy of the live
// effective configuration. Every runtime reader (UI, components,
// health) must go through this — never touch f.cfg directly.
//
// The published *config.Config is treated as immutable: each
// recomputeEffective builds a fresh value and swaps the pointer under
// the write lock, so a reader holding an earlier snapshot keeps a
// stable view. (A value copy is used at the boundary to make that
// invariant the caller's responsibility, not the frame's.)
func (f *Frame) effectiveConfig() config.Config {
	f.effMu.RLock()
	defer f.effMu.RUnlock()
	return *f.cfg
}

// overlayServerConfig writes non-zero server fields onto the config.
// Zero/empty server values are "unset" (fall through to the config
// file) — matching the server-side validation.
func overlayServerConfig(c *config.FrameConfig, sc sync.Config) {
	if sc.SlideshowIntervalSec > 0 {
		c.Slideshow.Interval = time.Duration(sc.SlideshowIntervalSec) * time.Second
	}
	if sc.Transition != "" {
		c.Slideshow.Transition = sc.Transition
	}
	if sc.TransitionDurationMS > 0 {
		c.Slideshow.TransitionDuration = time.Duration(sc.TransitionDurationMS) * time.Millisecond
	}
	if sc.SyncIntervalSec > 0 {
		c.Sync.ManifestInterval = time.Duration(sc.SyncIntervalSec) * time.Second
	}
	if sc.SleepOn != "" {
		c.Sleep.On = sc.SleepOn
	}
	if sc.SleepOff != "" {
		c.Sleep.Off = sc.SleepOff
	}
	if sc.Brightness != nil {
		c.Display.Brightness = float64(*sc.Brightness) / 100.0
	}
	if sc.Orientation != "" {
		switch sc.Orientation {
		case "normal":
			c.Display.Orientation = "0"
		case "rotate90":
			c.Display.Orientation = "90"
		case "rotate180":
			c.Display.Orientation = "180"
		case "rotate270":
			c.Display.Orientation = "270"
		}
	}
	if sc.ShowClock != nil {
		c.Slideshow.ShowClock = *sc.ShowClock
	}
	if sc.ShowCaptions != nil {
		c.Slideshow.ShowCaption = *sc.ShowCaptions
	}
	if sc.UpdateChannel != "" {
		c.Update.Channel = sc.UpdateChannel
	}
}

// applyEffectiveToUI pushes the effective config into the live UI
// engine, sleep schedule and driver. Safe to call from any goroutine.
func (f *Frame) applyEffectiveToUI(ctx context.Context) {
	f.ui.eng.ApplyExternalConfig(ui.ExternalConfig{
		Interval:    f.cfg.Frame.Slideshow.Interval,
		Transition:  f.cfg.Frame.Slideshow.Transition,
		ShowCaption: f.cfg.Frame.Slideshow.ShowCaption,
		ShowClock:   f.cfg.Frame.Slideshow.ShowClock,
		Brightness:  f.cfg.Frame.Display.Brightness,
		Orientation: f.orientationDegrees(),
		SleepOn:     hmToMinutes(f.cfg.Frame.Sleep.On),
		SleepOff:    hmToMinutes(f.cfg.Frame.Sleep.Off),
	})

	if err := f.drv.SetBrightness(ctx, f.cfg.Frame.Display.Brightness); err != nil {
		f.log.Warn("brightness apply failed", "error", err)
	}
	if err := f.drv.SetRotation(ctx, f.orientationDegrees()); err != nil {
		f.log.Warn("rotation apply failed", "error", err)
	}
	if sched, err := sleep.FromConfig(f.cfg.Frame.Sleep.On, f.cfg.Frame.Sleep.Off, f.cfg.Frame.Sleep.TZ); err == nil {
		f.ui.setSchedule(sched)
	}
	f.log.Info("effective config applied",
		"interval", f.cfg.Frame.Slideshow.Interval.String(),
		"transition", f.cfg.Frame.Slideshow.Transition,
		"brightness", f.cfg.Frame.Display.Brightness,
		"sleep", f.cfg.Frame.Sleep.On, "-", f.cfg.Frame.Sleep.Off)
}

// formatServerConfig renders a compact, secret-free summary for the
// frame state table (shown by `openframe status`).
func formatServerConfig(c sync.Config) string {
	parts := []string{}
	if c.Name != "" {
		parts = append(parts, "name="+c.Name)
	}
	if c.SlideshowIntervalSec > 0 {
		parts = append(parts, fmt.Sprintf("interval=%ds", c.SlideshowIntervalSec))
	}
	if c.Transition != "" {
		parts = append(parts, "transition="+c.Transition)
	}
	if c.SyncIntervalSec > 0 {
		parts = append(parts, fmt.Sprintf("sync=%ds", c.SyncIntervalSec))
	}
	if c.SleepOn != "" || c.SleepOff != "" {
		parts = append(parts, fmt.Sprintf("sleep=%s-%s", c.SleepOn, c.SleepOff))
	}
	if c.Brightness != nil {
		parts = append(parts, fmt.Sprintf("brightness=%d", *c.Brightness))
	}
	if c.UpdateChannel != "" {
		parts = append(parts, "channel="+c.UpdateChannel)
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

// uiSettingsFromConfig renders the effective config as the UI's
// staged-settings shape (used by tests and the settings screen init).
func uiSettingsFromConfig(c *config.FrameConfig) ui.Settings {
	return ui.Settings{
		Brightness:        c.Display.Brightness,
		SlideshowInterval: c.Slideshow.Interval,
		Transition:        c.Slideshow.Transition,
		Orientation:       orientationInt(c.Display.Orientation),
		SleepOn:           hmToMinutes(c.Sleep.On),
		SleepOff:          hmToMinutes(c.Sleep.Off),
		ShowCaption:       c.Slideshow.ShowCaption,
		ShowClock:         c.Slideshow.ShowClock,
	}
}

func orientationInt(s string) int {
	switch s {
	case "90":
		return 90
	case "180":
		return 180
	case "270":
		return 270
	default:
		return 0
	}
}
