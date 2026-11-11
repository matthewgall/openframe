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
	"time"

	"github.com/openframe/openframe/internal/config"
	"github.com/openframe/openframe/internal/frame/display"
	"github.com/openframe/openframe/internal/logging"
	"github.com/openframe/openframe/internal/version"
)

// State keys persisted in agent_state.
const (
	StateKeyRunning       = "running"        // "true"/"false"
	StateKeyLastHeartbeat = "last_heartbeat" // RFC3339Nano UTC
	StateKeyAppVersion    = "app_version"
	StateKeyLastSyncError = "last_sync_error" // set by M3 sync agent
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
	return &Frame{
		cfg:     cfg,
		log:     log,
		db:      dbh,
		drv:     drv,
		started: time.Now(),
		runner:  newRunner(log),
	}, nil
}

// Identity returns the device identity (valid after Run has started).
func (f *Frame) Identity() string { return f.identity }

// Driver exposes the display driver (used by M4 renderer and by tests).
func (f *Frame) Driver() display.Driver { return f.drv }

// DB exposes the frame database (used by `openframe status` and tests).
func (f *Frame) DB() *sql.DB { return f.db }

// Run starts the frame and blocks until ctx is cancelled.
//
// M1 component set:
//   - heartbeat: records liveness to agent_state (basis for status and
//     the M5 health reporter)
//
// M3 adds: manifest checker, download queue processor, local-changes
// uploader. M4 adds: renderer loop, sleep scheduler. M6 adds: update
// coordinator. They slot into the same bounded runner.
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

	err = f.runner.Run(ctx, []Component{
		{Name: "heartbeat", Run: f.heartbeatComponent},
	})

	// Mark ourselves stopped regardless of the cause.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := setState(stopCtx, f.db, StateKeyRunning, "false"); err != nil {
		log.Warn("failed to record stop state", "error", err)
	}

	log.InfoContext(ctx, "frame stopped", "device_id", identity, "error", errString(err))
	return err
}

func (f *Frame) heartbeat(ctx context.Context) {
	if err := setState(ctx, f.db, StateKeyLastHeartbeat, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		f.log.Warn("heartbeat failed", "error", err)
	}
}

func (f *Frame) heartbeatComponent(ctx context.Context) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			f.heartbeat(ctx)
		}
	}
}
