package display

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/matthewgall/openframe/internal/logging"
)

// Options configures driver construction.
type Options struct {
	// Name is the configured driver ("headless", "auto", "drm", ...).
	// "auto" tries the registered hardware backends in registration
	// order and falls back to headless when none is available (dev
	// boxes, CI).
	Name string
	// Width/Height are the requested frame dimensions, used by the
	// headless driver. Hardware backends may override them from the
	// panel's preferred mode (auto).
	Width  int
	Height int
	// Card is the DRM card for the drm backend (default
	// /dev/dri/card0).
	Card string
	Log  *slog.Logger
	// Snapshots configures the headless visual simulator (empty Dir
	// disables it).
	Snapshots SnapshotConfig
}

// DriverConstructor builds and initialises a named hardware backend.
type DriverConstructor func(ctx context.Context, log *slog.Logger, opts Options) (Driver, error)

var (
	regMu        sync.Mutex
	regOrder     []string
	constructors = map[string]DriverConstructor{}
)

// Register adds a hardware display backend under a name. Backends
// call this from init(); it is why the factory can construct the DRM
// driver without importing it (no import cycle: the backend imports
// display, not the reverse). "headless" and "auto" are reserved.
func Register(name string, c DriverConstructor) {
	regMu.Lock()
	defer regMu.Unlock()
	if name == "headless" || name == "auto" {
		panic("display: reserved driver name " + name)
	}
	if _, exists := constructors[name]; !exists {
		regOrder = append(regOrder, name)
	}
	constructors[name] = c
}

// New constructs a display driver from options and leaves it
// initialised (Init already called).
func New(ctx context.Context, opts Options) (Driver, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	// NewComponent (single With) keeps the JSON log valid: nested
	// .With("component", ...) calls would emit duplicate keys.
	log = logging.NewComponent(log, "frame.display")

	switch opts.Name {
	case "", "headless":
		d := headless(log, opts)
		if err := d.Init(ctx); err != nil {
			return nil, err
		}
		log.InfoContext(ctx, "display driver ready", "driver", d.Name())
		return d, nil

	case "auto":
		regMu.Lock()
		names := append([]string(nil), regOrder...)
		cs := make(map[string]DriverConstructor, len(names))
		for _, n := range names {
			cs[n] = constructors[n]
		}
		regMu.Unlock()
		for _, n := range names {
			d, err := cs[n](ctx, log, opts)
			if err != nil {
				log.Info("display driver auto: backend unavailable", "backend", n, "error", err)
				continue
			}
			log.InfoContext(ctx, "display driver ready", "driver", d.Name(), "detail", d.Info().Extra)
			return d, nil
		}
		log.InfoContext(ctx, "display driver auto: no hardware backend available, using headless dev renderer")
		d := headless(log, opts)
		if err := d.Init(ctx); err != nil {
			return nil, err
		}
		return d, nil

	default:
		regMu.Lock()
		c, ok := constructors[opts.Name]
		regMu.Unlock()
		if !ok {
			return nil, fmt.Errorf("%w: unknown driver %q", ErrUnsupportedDriver, opts.Name)
		}
		d, err := c(ctx, log, opts)
		if err != nil {
			return nil, err
		}
		log.InfoContext(ctx, "display driver ready", "driver", d.Name(), "detail", d.Info().Extra)
		return d, nil
	}
}

func headless(log *slog.Logger, opts Options) *Headless {
	return NewHeadlessWithSnapshots(log, opts.Width, opts.Height, opts.Snapshots)
}
