package display

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Headless is the development renderer: it keeps display state in memory
// and never touches hardware. It is the default driver on dev machines
// and the backend for frame integration tests.
//
// It also implements the documented fallbacks: brightness and power are
// accepted and stored (so agent code is identical across drivers) but
// have no physical effect.
type Headless struct {
	log *slog.Logger

	mu            sync.RWMutex
	width, height int
	brightness    float64
	power         bool
	last          *ImageRef
	initialized   bool
	closed        bool
	presents      int64
}

var _ Driver = (*Headless)(nil)

// NewHeadless builds the development driver.
func NewHeadless(log *slog.Logger, width, height int) *Headless {
	if width <= 0 {
		width = 1280
	}
	if height <= 0 {
		height = 800
	}
	return &Headless{
		log:        log,
		width:      width,
		height:     height,
		brightness: 0.8,
		power:      true,
	}
}

func (h *Headless) Name() string { return "headless" }

func (h *Headless) Info() Info {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return Info{
		Name:               "headless",
		ResolutionWidth:    h.width,
		ResolutionHeight:   h.height,
		SupportsBrightness: false,
		SupportsPower:      false,
	}
}

func (h *Headless) Init(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return fmt.Errorf("headless driver: closed")
	}
	h.initialized = true
	return nil
}

func (h *Headless) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	h.initialized = false
	return nil
}

func (h *Headless) Present(ctx context.Context, img ImageRef) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.initialized {
		return fmt.Errorf("headless driver: not initialized")
	}
	if img.Path == "" {
		return fmt.Errorf("headless driver: empty image path")
	}
	h.last = &img
	h.presents++
	h.log.DebugContext(ctx, "headless: image presented",
		"path", img.Path, "hash", img.Hash, "n", h.presents)
	return nil
}

func (h *Headless) SetBrightness(ctx context.Context, level float64) error {
	if level < 0 || level > 1 {
		return fmt.Errorf("brightness out of range: %v", level)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.brightness = level
	return nil
}

func (h *Headless) SetPower(ctx context.Context, on bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.power = on
	h.log.DebugContext(ctx, "headless: display power", "on", on)
	return nil
}

// LastPresented returns the most recently presented image (test/doctor
// helper; nil until the first Present).
func (h *Headless) LastPresented() *ImageRef {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.last == nil {
		return nil
	}
	cp := *h.last
	return &cp
}

// Power reports the logical display power state.
func (h *Headless) Power() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.power
}

// Presents counts Present calls since Init (test helper).
func (h *Headless) Presents() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.presents
}
