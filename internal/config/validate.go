package config

import (
	"fmt"
	"regexp"
	"time"
)

var timeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// Validate checks semantic correctness of the loaded configuration.
// It is deliberately strict for the fields that a misconfiguration would
// make dangerous (window times, driver names, sizes), and permissive
// elsewhere.
func (c *Config) Validate() error {
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen must not be empty")
	}
	if c.Server.ReadTimeout < 0 || c.Server.WriteTimeout < 0 || c.Server.ShutdownTimeout < 0 {
		return fmt.Errorf("server timeouts must be >= 0")
	}
	if c.Server.SessionTTL < time.Hour {
		return fmt.Errorf("server.session_ttl must be >= 1h")
	}
	if c.Server.MaxUploadBytes < 1<<20 || c.Server.MaxUploadBytes > 1<<30 {
		return fmt.Errorf("server.max_upload_bytes must be in [1 MiB, 1 GiB]")
	}
	if c.Server.MaxUploadFiles < 1 || c.Server.MaxUploadFiles > 500 {
		return fmt.Errorf("server.max_upload_files must be in [1, 500]")
	}
	if c.Server.MaxDecodeDimension < 1024 || c.Server.MaxDecodeDimension > 65536 {
		return fmt.Errorf("server.max_decode_dimension must be in [1024, 65536]")
	}
	if c.Server.LoginRateLimitPerMin < 1 || c.Server.LoginRateLimitPerMin > 1000 {
		return fmt.Errorf("server.login_rate_limit_per_min must be in [1, 1000]")
	}

	d := c.Frame.Display
	if d.Driver != "headless" && d.Driver != "auto" && d.Driver != "drm" {
		return fmt.Errorf("frame.display.driver %q is not supported in this release (headless or auto)", d.Driver)
	}
	if d.Width <= 0 || d.Height <= 0 || d.Width > 8192 || d.Height > 8192 {
		return fmt.Errorf("frame.display.width/height must be in (0, 8192]")
	}
	if d.Brightness < 0 || d.Brightness > 1 {
		return fmt.Errorf("frame.display.brightness must be in [0, 1]")
	}
	switch d.Orientation {
	case "auto", "0", "90", "180", "270":
	default:
		return fmt.Errorf("frame.display.orientation %q invalid (auto|0|90|180|270)", d.Orientation)
	}

	s := c.Frame.Slideshow
	if s.Interval < 5*time.Second {
		return fmt.Errorf("frame.slideshow.interval must be >= 5s (shorter intervals thrash the display)")
	}
	switch s.Order {
	case "sequential", "random":
	default:
		return fmt.Errorf("frame.slideshow.order %q invalid (sequential|random)", s.Order)
	}
	switch s.Fit {
	case "contain", "cover", "crop":
	default:
		return fmt.Errorf("frame.slideshow.fit %q invalid (contain|cover|crop)", s.Fit)
	}

	sy := c.Frame.Sync
	if sy.ManifestInterval < time.Minute {
		return fmt.Errorf("frame.sync.manifest_interval must be >= 1m")
	}
	if err := validateWindow(&sy.DownloadWindow, "frame.sync.download_window"); err != nil {
		return err
	}
	switch sy.Policy {
	case "immediate", "scheduled", "hybrid":
	default:
		return fmt.Errorf("frame.sync.policy %q invalid (immediate|scheduled|hybrid)", sy.Policy)
	}
	if sy.MaxConcurrent < 1 || sy.MaxConcurrent > 16 {
		return fmt.Errorf("frame.sync.max_concurrent must be in [1, 16]")
	}
	if sy.MaxDownloadBytes <= 0 || sy.MaxDownloadBytes > 2<<30 {
		return fmt.Errorf("frame.sync.max_download_bytes must be in (0, 2 GiB]")
	}
	if sy.HealthReportEvery < time.Minute {
		return fmt.Errorf("frame.sync.health_report_interval must be >= 1m")
	}

	sleep := c.Frame.Sleep.TZWindow()
	if err := validateWindow(&sleep, "frame.sleep"); err != nil {
		return err
	}

	u := c.Frame.Update
	switch u.Channel {
	case "stable", "beta", "development":
	default:
		return fmt.Errorf("frame.update.channel %q invalid (stable|beta|development)", u.Channel)
	}
	switch u.Policy {
	case "automatic", "download_only", "manual":
	default:
		return fmt.Errorf("frame.update.policy %q invalid (automatic|download_only|manual)", u.Policy)
	}
	if err := validateWindow(&u.MaintenanceWindow, "frame.update.maintenance_window"); err != nil {
		return err
	}
	if u.CheckInterval < time.Minute {
		return fmt.Errorf("frame.update.check_interval must be >= 1m")
	}

	if c.Frame.Storage.QuotaBytes < 0 {
		return fmt.Errorf("frame.storage.quota_bytes must be >= 0")
	}
	return nil
}

// TZWindow is a helper so sleep (on/off pair) can reuse window validation.
func (s SleepConfig) TZWindow() Window {
	return Window{Start: s.On, End: s.Off, TZ: s.TZ}
}

func validateWindow(w *Window, name string) error {
	if !timeRe.MatchString(w.Start) {
		return fmt.Errorf("%s.start %q must be HH:MM (24h)", name, w.Start)
	}
	if !timeRe.MatchString(w.End) {
		return fmt.Errorf("%s.end %q must be HH:MM (24h)", name, w.End)
	}
	if w.TZ == "" {
		return fmt.Errorf("%s.tz must not be empty", name)
	}
	if _, err := time.LoadLocation(w.TZ); err != nil {
		return fmt.Errorf("%s.tz %q is not a known IANA zone: %w", name, w.TZ, err)
	}
	return nil
}
