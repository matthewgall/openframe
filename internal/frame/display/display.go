// Package display defines the frame display driver boundary.
//
// The renderer (milestone 4) talks to a Driver; the frame agent and the
// rest of the system never touch graphics directly. This keeps the
// server binary free of any graphics linkage and lets hardware
// backends (DRM/KMS, SDL2) be added behind this interface without
// changing the agent, sync engine or UI logic (ADR-0006).
//
// M1 shipped the headless development driver. M4 adds PresentFrame:
// the UI composites everything (photograph, captions, overlays,
// transitions) into a single fullscreen *image.NRGBA canvas and the
// driver's only job is to make those pixels visible. The headless
// driver doubles as the visual simulator (spec §10 "development
// renderer that works without physical touchscreen hardware") when a
// snapshot directory is configured: it writes each frame as a PNG so
// the full UI is observable without a screen.
package display

import (
	"context"
	"fmt"
	"image"
)

// Info describes driver capabilities.
type Info struct {
	Name               string `json:"name"`
	ResolutionWidth    int    `json:"resolution_width"`
	ResolutionHeight   int    `json:"resolution_height"`
	SupportsBrightness bool   `json:"supports_brightness"`
	SupportsPower      bool   `json:"supports_power"`
	// Extra is driver-specific detail (e.g. the DRM card/connector
	// in use), for doctor output.
	Extra string `json:"extra,omitempty"`
}

// Driver is the hardware/backend abstraction for the frame display.
//
// Implementations must be safe for concurrent use, must not block
// indefinitely, and must treat Init/Close as idempotent from the
// caller's perspective (double Close is a no-op, Init after Close is
// an error).
type Driver interface {
	// Name returns the driver identifier ("headless", "drm", ...).
	Name() string

	// Info reports capabilities.
	Info() Info

	// FrameSize reports the fullscreen frame dimensions the UI must
	// render at.
	FrameSize() (width, height int)

	// Init prepares the backend (opens the device, sets mode, ...).
	Init(ctx context.Context) error

	// Close releases the backend.
	Close() error

	// PresentFrame makes img the fullscreen image. img must be exactly
	// the driver's resolution; the driver copies the pixels (the UI
	// reuses its canvas immediately after). The driver owns nothing
	// about the image contents — transitions, fit modes and overlays
	// are all already composited by the renderer (spec §10).
	PresentFrame(ctx context.Context, img *image.NRGBA) error

	// SetBrightness sets display brightness in [0,1]. Drivers without
	// hardware backlight control degrade to no-op and must not error
	// (spec §11: fallback where hardware brightness control is
	// unavailable).
	SetBrightness(ctx context.Context, level float64) error

	// SetPower turns the display on/off. Where the hardware exposes a
	// real backlight switch it is used; otherwise the driver falls
	// back to presenting a black frame. The frame agent keeps running
	// while the display is off (spec §11).
	SetPower(ctx context.Context, on bool) error

	// SetRotation applies a screen rotation (0/90/180/270) where the
	// hardware supports it; drivers without support no-op.
	SetRotation(ctx context.Context, degrees int) error
}

// ErrUnsupportedDriver is returned by New when a driver name is not
// available in this release.
var ErrUnsupportedDriver = fmt.Errorf("display driver not available in this release")
