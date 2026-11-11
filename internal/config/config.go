// Package config loads OpenFrame configuration.
//
// Precedence (lowest to highest):
//
//  1. built-in defaults
//  2. YAML file (if provided via --config)
//  3. environment variables (OPENFRAME_*)
//
// Environment variable names mirror the YAML path with the OPENFRAME_
// prefix, uppercased and dots/dashes replaced by underscores, e.g.
// server.listen -> OPENFRAME_SERVER_LISTEN. See docs/development.md.
package config

import (
	"os"
	"path/filepath"
	"time"
)

// Config is the root configuration document.
type Config struct {
	// DataDir is the root state directory. On Raspberry Pi deployments this
	// is /var/lib/openframe; in development it defaults to ~/.openframe.
	// Sub-layout:
	//
	//	DataDir/server   server database + original photo storage
	//	DataDir/frame    frame database + content-addressed photo cache
	//	DataDir/updates  updater state + A/B slots staging area
	DataDir string `yaml:"data_dir"`

	Log    LogConfig    `yaml:"log"`
	Server ServerConfig `yaml:"server"`
	Frame  FrameConfig  `yaml:"frame"`
}

type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | text
	Output string `yaml:"output"` // stderr | stdout
}

type ServerConfig struct {
	Listen string `yaml:"listen"` // address:port
	// PublicBaseURL is the externally reachable base URL of this server
	// (used in pairing links and manifest download URLs). May be empty in
	// development; frames can then be pointed at Listen directly.
	PublicBaseURL   string        `yaml:"public_base_url"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

type FrameConfig struct {
	// ServerURL is the management server base URL, e.g.
	// https://frame.example.com. Empty means the frame is not yet
	// configured; the renderer keeps working offline.
	ServerURL string `yaml:"server_url"`

	Display   DisplayConfig   `yaml:"display"`
	Slideshow SlideshowConfig `yaml:"slideshow"`
	Sync      SyncConfig      `yaml:"sync"`
	Sleep     SleepConfig     `yaml:"sleep"`
	Update    UpdateConfig    `yaml:"update"`
	Storage   StorageConfig   `yaml:"storage"`
}

type DisplayConfig struct {
	// Driver selects the display backend:
	//   headless - development renderer, no hardware (default in dev)
	//   auto     - pick a hardware backend if one is available
	// Hardware drivers (drm, ...) arrive in milestone 4 and are selected
	// here by name; see internal/frame/display and ADR-0006.
	Driver      string  `yaml:"driver"`
	Width       int     `yaml:"width"`
	Height      int     `yaml:"height"`
	Brightness  float64 `yaml:"brightness"`  // 0.0 - 1.0
	Orientation string  `yaml:"orientation"` // auto | 0 | 90 | 180 | 270
}

type SlideshowConfig struct {
	Interval    time.Duration `yaml:"interval"` // default 45s
	Order       string        `yaml:"order"`    // sequential | random
	Fit         string        `yaml:"fit"`      // contain | cover | crop
	ShowCaption bool          `yaml:"show_caption"`
	ShowClock   bool          `yaml:"show_clock"`
}

type SyncConfig struct {
	// ManifestInterval is how often the agent asks the server whether a
	// newer manifest exists (conditional GET, ETag based).
	ManifestInterval time.Duration `yaml:"manifest_interval"`
	// DownloadWindow is the daily window in which non-priority downloads
	// run (local time).
	DownloadWindow Window `yaml:"download_window"`
	// Policy: immediate | scheduled | hybrid.
	// hybrid downloads small changes right away and defers large batches
	// until DownloadWindow.
	Policy            string        `yaml:"policy"`
	HybridMaxBytes    int64         `yaml:"hybrid_max_bytes"`
	MaxConcurrent     int           `yaml:"max_concurrent"`
	MaxDownloadBytes  int64         `yaml:"max_download_bytes"`
	HealthReportEvery time.Duration `yaml:"health_report_interval"`
}

// Window is a daily time window in a fixed IANA time zone.
type Window struct {
	Start string `yaml:"start"` // "01:00"
	End   string `yaml:"end"`   // "06:00"
	TZ    string `yaml:"tz"`    // e.g. Europe/London
}

type SleepConfig struct {
	On  string `yaml:"on"`  // "07:00"
	Off string `yaml:"off"` // "22:00"
	TZ  string `yaml:"tz"`  // e.g. Europe/London
}

type UpdateConfig struct {
	// Channel: stable | beta | development
	Channel string `yaml:"channel"`
	// Policy: automatic | download_only | manual
	Policy            string        `yaml:"policy"`
	MaintenanceWindow Window        `yaml:"maintenance_window"`
	CheckInterval     time.Duration `yaml:"check_interval"`
}

type StorageConfig struct {
	// QuotaBytes caps the frame's photo cache. 0 means "use the free
	// space heuristic" (keep >= 1 GiB free on the volume).
	QuotaBytes int64 `yaml:"quota_bytes"`
}

// Paths returns the resolved sub-directories under DataDir.
func (c *Config) Paths() (serverDir, frameDir, updatesDir string) {
	serverDir = filepath.Join(c.DataDir, "server")
	frameDir = filepath.Join(c.DataDir, "frame")
	updatesDir = filepath.Join(c.DataDir, "updates")
	return
}

// Default returns a Config with safe defaults.
func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		DataDir: filepath.Join(home, ".openframe"),
		Log:     LogConfig{Level: "info", Format: "json", Output: "stderr"},
		Server: ServerConfig{
			Listen:          ":8080",
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    60 * time.Second,
			ShutdownTimeout: 10 * time.Second,
		},
		Frame: FrameConfig{
			Display: DisplayConfig{
				Driver:      "headless",
				Width:       1280,
				Height:      800,
				Brightness:  0.8,
				Orientation: "auto",
			},
			Sync: SyncConfig{
				ManifestInterval:  60 * time.Minute,
				DownloadWindow:    Window{Start: "01:00", End: "06:00", TZ: "Europe/London"},
				Policy:            "hybrid",
				HybridMaxBytes:    50 * 1024 * 1024, // 50 MiB
				MaxConcurrent:     2,
				MaxDownloadBytes:  200 * 1024 * 1024, // 200 MiB per file
				HealthReportEvery: 15 * time.Minute,
			},
			Slideshow: SlideshowConfig{
				Interval:    45 * time.Second,
				Order:       "sequential",
				Fit:         "contain",
				ShowCaption: true,
				ShowClock:   true,
			},
			Sleep: SleepConfig{On: "07:00", Off: "22:00", TZ: "Europe/London"},
			Update: UpdateConfig{
				Channel:           "stable",
				Policy:            "automatic",
				MaintenanceWindow: Window{Start: "02:00", End: "05:00", TZ: "Europe/London"},
				CheckInterval:     6 * time.Hour,
			},
			Storage: StorageConfig{},
		},
	}
}
