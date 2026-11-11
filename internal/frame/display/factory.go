package display

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/openframe/openframe/internal/logging"
)

// Options configures driver construction.
type Options struct {
	// Name is the configured driver ("headless", "auto", "drm", ...).
	// "auto" selects the best hardware backend available, falling back
	// to headless when none is (dev boxes, CI).
	Name   string
	Width  int
	Height int
	Log    *slog.Logger
}

// New constructs a display driver from options.
//
// M1 availability:
//
//	headless - always available (dev renderer / test simulator)
//	auto     - resolves to headless in M1; in M4 it probes for a DRM
//	            device and falls back to headless when none is found
//	drm      - milestone 4 (DRM/KMS kiosk backend, see ADR-0006)
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
		d := NewHeadless(log, opts.Width, opts.Height)
		if err := d.Init(ctx); err != nil {
			return nil, err
		}
		log.InfoContext(ctx, "display driver ready", "driver", d.Name())
		return d, nil
	case "auto":
		// M1: no hardware drivers compiled in yet.
		log.InfoContext(ctx, "display driver auto: no hardware backend in this release, using headless dev renderer")
		d := NewHeadless(log, opts.Width, opts.Height)
		if err := d.Init(ctx); err != nil {
			return nil, err
		}
		return d, nil
	case "drm":
		return nil, fmt.Errorf("%w: drm backend arrives in milestone 4 (ADR-0006)", ErrUnsupportedDriver)
	default:
		return nil, fmt.Errorf("%w: unknown driver %q", ErrUnsupportedDriver, opts.Name)
	}
}
