package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultValues(t *testing.T) {
	cfg := Default()
	if cfg.Server.Listen != ":8080" {
		t.Errorf("default listen = %q, want :8080", cfg.Server.Listen)
	}
	if cfg.Frame.Sync.ManifestInterval != 60*time.Minute {
		t.Errorf("default manifest interval = %v, want 60m", cfg.Frame.Sync.ManifestInterval)
	}
	if cfg.Frame.Slideshow.Interval != 45*time.Second {
		t.Errorf("default slideshow interval = %v, want 45s", cfg.Frame.Slideshow.Interval)
	}
	if cfg.Frame.Sync.Policy != "hybrid" {
		t.Errorf("default sync policy = %q, want hybrid", cfg.Frame.Sync.Policy)
	}
	if cfg.Frame.Update.Channel != "stable" {
		t.Errorf("default update channel = %q, want stable", cfg.Frame.Update.Channel)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults should validate: %v", err)
	}
}

func TestLoadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openframe.yaml")
	yaml := `
data_dir: /tmp/custom
server:
  listen: ":9090"
  read_timeout: 12s
frame:
  server_url: https://frame.example.com
  sync:
    manifest_interval: 30m
    policy: scheduled
  sleep:
    on: "08:00"
    off: "23:00"
    tz: Europe/Berlin
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/tmp/custom" {
		t.Errorf("data_dir = %q", cfg.DataDir)
	}
	if cfg.Server.Listen != ":9090" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Server.ReadTimeout != 12*time.Second {
		t.Errorf("read_timeout = %v", cfg.Server.ReadTimeout)
	}
	if cfg.Frame.ServerURL != "https://frame.example.com" {
		t.Errorf("server_url = %q", cfg.Frame.ServerURL)
	}
	if cfg.Frame.Sync.ManifestInterval != 30*time.Minute {
		t.Errorf("manifest_interval = %v", cfg.Frame.Sync.ManifestInterval)
	}
	if cfg.Frame.Sleep.On != "08:00" || cfg.Frame.Sleep.TZ != "Europe/Berlin" {
		t.Errorf("sleep = %+v", cfg.Frame.Sleep)
	}
	// Unset values keep defaults.
	if cfg.Frame.Display.Driver != "headless" {
		t.Errorf("driver default = %q", cfg.Frame.Display.Driver)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("OPENFRAME_DATA_DIR", "/tmp/from-env")
	t.Setenv("OPENFRAME_SERVER_LISTEN", ":7070")
	t.Setenv("OPENFRAME_LOG_LEVEL", "debug")
	t.Setenv("OPENFRAME_FRAME_SYNC_MANIFEST_INTERVAL", "45m")
	t.Setenv("OPENFRAME_FRAME_SYNC_MAX_CONCURRENT", "4")
	t.Setenv("OPENFRAME_FRAME_DISPLAY_BRIGHTNESS", "0.5")
	t.Setenv("OPENFRAME_FRAME_SERVER_URL", "https://env.example.com")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/tmp/from-env" {
		t.Errorf("env data_dir = %q", cfg.DataDir)
	}
	if cfg.Server.Listen != ":7070" {
		t.Errorf("env listen = %q", cfg.Server.Listen)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("env log level = %q", cfg.Log.Level)
	}
	if cfg.Frame.Sync.ManifestInterval != 45*time.Minute {
		t.Errorf("env manifest_interval = %v", cfg.Frame.Sync.ManifestInterval)
	}
	if cfg.Frame.Sync.MaxConcurrent != 4 {
		t.Errorf("env max_concurrent = %d", cfg.Frame.Sync.MaxConcurrent)
	}
	if cfg.Frame.Display.Brightness != 0.5 {
		t.Errorf("env brightness = %v", cfg.Frame.Display.Brightness)
	}
	if cfg.Frame.ServerURL != "https://env.example.com" {
		t.Errorf("env server_url = %q", cfg.Frame.ServerURL)
	}
}

func TestEnvBeatsYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte("server:\n  listen: \":1111\"\n"), 0o600)
	t.Setenv("OPENFRAME_SERVER_LISTEN", ":2222")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":2222" {
		t.Errorf("env should beat yaml, got %q", cfg.Server.Listen)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty datadir", func(c *Config) { c.DataDir = "" }},
		{"bad driver", func(c *Config) { c.Frame.Display.Driver = "vulkan" }},
		{"brightness high", func(c *Config) { c.Frame.Display.Brightness = 2 }},
		{"bad orientation", func(c *Config) { c.Frame.Display.Orientation = "45" }},
		{"slideshow too short", func(c *Config) { c.Frame.Slideshow.Interval = time.Second }},
		{"bad slideshow order", func(c *Config) { c.Frame.Slideshow.Order = "shuffle" }},
		{"bad fit", func(c *Config) { c.Frame.Slideshow.Fit = "stretch" }},
		{"manifest too frequent", func(c *Config) { c.Frame.Sync.ManifestInterval = 10 * time.Second }},
		{"bad download window start", func(c *Config) { c.Frame.Sync.DownloadWindow.Start = "25:00" }},
		{"bad download window end", func(c *Config) { c.Frame.Sync.DownloadWindow.End = "06:60" }},
		{"bad tz", func(c *Config) { c.Frame.Sync.DownloadWindow.TZ = "Mars/Olympus" }},
		{"bad policy", func(c *Config) { c.Frame.Sync.Policy = "whenever" }},
		{"max concurrent 0", func(c *Config) { c.Frame.Sync.MaxConcurrent = 0 }},
		{"max download 0", func(c *Config) { c.Frame.Sync.MaxDownloadBytes = 0 }},
		{"health too frequent", func(c *Config) { c.Frame.Sync.HealthReportEvery = 10 * time.Second }},
		{"bad sleep on", func(c *Config) { c.Frame.Sleep.On = "7:00" }},
		{"bad channel", func(c *Config) { c.Frame.Update.Channel = "nightly" }},
		{"bad update policy", func(c *Config) { c.Frame.Update.Policy = "auto" }},
		{"bad maintenance window", func(c *Config) { c.Frame.Update.MaintenanceWindow.End = "5:00" }},
		{"negative quota", func(c *Config) { c.Frame.Storage.QuotaBytes = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestPaths(t *testing.T) {
	cfg := Default()
	cfg.DataDir = "/var/lib/openframe"
	s, f, u := cfg.Paths()
	if s != "/var/lib/openframe/server" || f != "/var/lib/openframe/frame" || u != "/var/lib/openframe/updates" {
		t.Errorf("paths = %q %q %q", s, f, u)
	}
}
