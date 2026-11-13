// Package frame implements the frame-side application: device identity,
// the background agent (heartbeat in M1; synchronisation in M3, update
// coordination in M6) and display driver wiring.
//
// Responsibilities are separated by construction (spec §7): the agent
// (this package + sync/) owns network and storage state, while the
// display package owns pixels. A renderer failure therefore cannot
// corrupt synchronisation state, and a sync failure cannot take the
// display down: each reports through its own component in the bounded
// runner.
package frame

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	gosync "sync"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/frame/cache"
	"github.com/matthewgall/openframe/internal/frame/display"
	"github.com/matthewgall/openframe/internal/frame/sync"
	"github.com/matthewgall/openframe/internal/logging"
	"github.com/matthewgall/openframe/internal/version"
)

// State keys persisted in agent_state.
const (
	StateKeyRunning       = "running"        // "true"/"false"
	StateKeyLastHeartbeat = "last_heartbeat" // RFC3339Nano UTC
	StateKeyAppVersion    = "app_version"
	StateKeyDeviceName    = "device_name"    // server-assigned name (M3 manifest config)
	StateKeyServerConfig  = "server_config"  // M5: summary of the last server config
	StateKeyUpdateChannel = "update_channel" // M6: effective update channel (read by the root updater)
)

// heartbeatInterval is how often the agent records a live heartbeat into
// the local database. It is deliberately short: `openframe status` and
// the M5 health reporter both derive freshness from it.
const heartbeatInterval = 30 * time.Second

// Frame is the frame application instance.
type Frame struct {
	cfg      *config.Config
	log      *slog.Logger
	db       *sql.DB
	drv      display.Driver
	identity string
	started  time.Time
	runner   *runner
	cache    *cache.Store
	engine   *sync.Engine
	ui       *uiState

	// M5 effective config (config file < server < local).
	effMu        gosync.RWMutex
	baseCfg      config.Config // pristine config-file values
	serverCfg    sync.Config   // last server-pushed per-frame config
	hasServerCfg bool

	// M5 health: bounded recent-error summary + present-error tracking.
	errors         *errorRing
	lastPresentErr string

	// M5 pairing: persisted credential file path.
	credPath string
}

// New wires a Frame. Callers own the db and driver lifetimes; Run stops
// the components but the caller still closes the db/driver after Run
// returns so `openframe status` can remain available during shutdown.
func New(cfg *config.Config, log *slog.Logger, dbh *sql.DB, drv display.Driver) (*Frame, error) {
	if cfg == nil {
		return nil, fmt.Errorf("frame: nil config")
	}
	if dbh == nil {
		return nil, fmt.Errorf("frame: nil db")
	}
	if drv == nil {
		return nil, fmt.Errorf("frame: nil display driver")
	}
	if log == nil {
		log = slog.Default()
	}
	log = logging.NewComponent(log, "frame")
	_, frameDir, _ := cfg.Paths()
	return &Frame{
		cfg:      cfg,
		log:      log,
		db:       dbh,
		drv:      drv,
		started:  time.Now(),
		runner:   newRunner(log),
		baseCfg:  *cfg,
		errors:   &errorRing{},
		credPath: filepath.Join(frameDir, credentialFileName),
	}, nil
}

// Identity returns the device identity (valid after Run has started).
func (f *Frame) Identity() string { return f.identity }

// Driver exposes the display driver (used by M4 renderer and by tests).
func (f *Frame) Driver() display.Driver { return f.drv }

// DB exposes the frame database (used by `openframe status` and tests).
func (f *Frame) DB() *sql.DB { return f.db }

// Cache exposes the content-addressed photo cache (used by the
// slideshow and by tests).
func (f *Frame) Cache() *cache.Store { return f.cache }

// Engine exposes the synchronisation engine (used by the status
// command, the M5 health reporter and tests). Nil until Run.
func (f *Frame) Engine() *sync.Engine { return f.engine }

// Run starts the frame and blocks until ctx is cancelled.
//
// Component set (bounded runner, spec §7):
//   - heartbeat: records liveness to agent_state (basis for status
//     and the M5 health reporter)
//   - ui (M4): the touchscreen interface — slideshow, gallery,
//     search, settings; presents the active collection at the
//     configured interval and runs regardless of connectivity
//   - touch (M4): evdev touchscreen → gestures into the UI
//   - sleep (M4): daily display sleep schedule (frame keeps running)
//   - thermal (M4): overheat throttling (slideshow interval doubling)
//   - sync.manifest / sync.downloads / sync.changes (M3): the
//     offline-first synchronisation engine, independently scheduled
//     per spec §9. M6 adds the update coordinator.
func (f *Frame) Run(ctx context.Context) error {
	log := f.log

	identity, err := EnsureIdentity(ctx, f.db)
	if err != nil {
		return fmt.Errorf("ensure identity: %w", err)
	}
	f.identity = identity
	log.InfoContext(ctx, "frame starting",
		"device_id", identity,
		"version", version.Version,
		"display", f.drv.Name(),
		"server_url", f.cfg.Frame.ServerURL,
	)

	if err := setState(ctx, f.db, StateKeyAppVersion, version.Version); err != nil {
		return err
	}
	if err := setState(ctx, f.db, StateKeyRunning, "true"); err != nil {
		return err
	}
	f.heartbeat(ctx) // record the first one immediately

	// M3: content-addressed cache + synchronisation engine. The cache
	// root lives under the frame data dir (DataDir/frame/photos).
	_, frameDir, _ := f.cfg.Paths()
	c, err := cache.New(filepath.Join(frameDir, "photos"), f.cfg.Frame.Storage.QuotaBytes)
	if err != nil {
		return fmt.Errorf("open photo cache: %w", err)
	}
	f.cache = c
	token := f.resolveToken()
	eng, err := sync.New(identity, f.cfg.Frame.ServerURL, token,
		f.log, f.db, c, &f.cfg.Frame)
	if err != nil {
		return fmt.Errorf("build sync engine: %w", err)
	}
	f.engine = eng
	if !eng.Paired() {
		log.Info("frame not paired; the pairing flow (or manual credential) will connect it")
	}

	// M4: display stack (UI engine, touch, sleep, thermal). The UI
	// reads only the active collection, so building it before the
	// first sync is safe: it shows the empty state until the first
	// activation arrives.
	if _, err := f.buildDisplay(ctx); err != nil {
		return fmt.Errorf("build display: %w", err)
	}

	// M5: server-pushed per-frame config (spec §6) applies to the UI,
	// sleep schedule and sync interval without a restart.
	eng.OnConfig(f.applyServerConfig)
	if err := f.recomputeEffective(ctx); err != nil {
		return fmt.Errorf("effective config: %w", err)
	}

	components := []Component{
		{Name: "heartbeat", Run: f.heartbeatComponent},
		{Name: "pairing", Run: f.pairingComponent},
		{Name: "ui", Run: f.uiComponent},
		{Name: "touch", Run: f.touchComponent},
		{Name: "sleep", Run: f.sleepComponent},
		{Name: "thermal", Run: f.thermalComponent},
	}
	for _, c := range eng.Components() {
		components = append(components, Component{Name: c.Name, Run: c.Run})
	}

	err = f.runner.Run(ctx, components)

	// Mark ourselves stopped regardless of the cause.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := setState(stopCtx, f.db, StateKeyRunning, "false"); err != nil {
		log.Warn("failed to record stop state", "error", err)
	}

	log.InfoContext(ctx, "frame stopped", "device_id", identity, "error", errString(err))
	return err
}

// resolveToken picks the device credential: an operator-pinned
// frame.device_token (config/env) wins; otherwise the credential
// persisted by the pairing flow (M5).
func (f *Frame) resolveToken() string {
	if f.cfg.Frame.DeviceToken != "" {
		return f.cfg.Frame.DeviceToken
	}
	cred, err := LoadCredential(f.credPath)
	if err != nil {
		f.log.Warn("credential file unreadable; running unpaired", "error", err)
		return ""
	}
	if cred == nil {
		return ""
	}
	if cred.DeviceID != f.identity {
		f.log.Warn("credential belongs to a different identity; running unpaired",
			"credential_device", cred.DeviceID, "identity", f.identity)
		return ""
	}
	f.log.Info("using persisted credential", "paired_via", cred.PairedVia)
	return cred.Token
}

// heartbeat records one liveness tick into agent_state.
func (f *Frame) heartbeat(ctx context.Context) {
	if err := setState(ctx, f.db, StateKeyLastHeartbeat, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		f.log.Warn("heartbeat failed", "error", err)
	}
}
