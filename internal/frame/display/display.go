// Package display defines the frame display driver boundary.
//
// The renderer (milestone 4) talks to a Driver; the frame agent and the
// rest of the system never touch graphics directly. This keeps the
// server binary free of any graphics linkage and lets hardware backends
// (DRM/KMS, SDL2) be added behind this interface without changing the
// agent, sync engine or UI logic (ADR-0006).
//
// M1 ships the headless development driver, which is also the renderer
// used for integration tests on Linux AMD64 (spec §20 "Create a
// simulator for frame behaviour on Linux AMD64").
package display

import (
	"context"
	"fmt"
)

// ImageRef identifies a local file to display. The renderer is expected
// to decode and scale with bounded memory; the ref carries a content
// hash so the renderer can detect a file that changed underneath it.
type ImageRef struct {
	Path string `json:"path"`
	Hash string `json:"hash,omitempty"` // hex sha256, optional
}

// Info describes driver capabilities.
type Info struct {
	Name               string `json:"name"`
	ResolutionWidth    int    `json:"resolution_width"`
	ResolutionHeight   int    `json:"resolution_height"`
	SupportsBrightness bool   `json:"supports_brightness"`
	SupportsPower      bool   `json:"supports_power"`
}

// Driver is the hardware/backend abstraction for the frame display.
//
// Implementations must be safe for concurrent use, must not block
// indefinitely, and must treat Init/Close as idempotent from the
// caller's perspective (double Close is a no-op, Init after Close is an
// error).
type Driver interface {
	// Name returns the driver identifier ("headless", "drm", ...).
	Name() string

	// Info reports capabilities.
	Info() Info

	// Init prepares the backend (opens the device, sets mode, ...).
	Init(ctx context.Context) error

	// Close releases the backend.
	Close() error

	// Present makes img the fullscreen image. The renderer owns
	// transitions, fit modes and overlays; Present is the atomic
	// "show this file" operation.
	Present(ctx context.Context, img ImageRef) error

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
}

// ErrUnsupportedDriver is returned by New when a driver name is not
// available in this release.
var ErrUnsupportedDriver = fmt.Errorf("display driver not available in this release")
