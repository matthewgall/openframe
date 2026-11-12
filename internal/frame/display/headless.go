package display

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Headless is the development renderer (ADR-0006): it keeps display
// state in memory and never touches hardware. It is the default driver
// on dev machines and the backend for frame integration tests.
//
// With SnapshotDir set it is also the *visual simulator* (spec §10):
// every frame is written to <dir>/latest.png (atomic), plus a
// timestamped ring of at most ringSize frames, so the complete UI —
// photographs, captions, overlays, transitions — can be watched on a
// machine without a screen (e.g. `tail -f` a directory, or wire it
// into a dev dashboard).
//
// It implements the documented fallbacks: brightness, power and
// rotation are accepted and stored (so agent code is identical across
// drivers) but have no physical effect (spec §11).
type Headless struct {
	log *slog.Logger

	snapDir      string
	snapInterval time.Duration
	ringSize     int
	ringName     [ringSize]string

	mu            sync.RWMutex
	width, height int
	brightness    float64
	power         bool
	rotation      int
	last          *image.NRGBA
	lastSnapAt    time.Time
	initialized   bool
	closed        bool
	presents      int64
}

const ringSize = 12

var _ Driver = (*Headless)(nil)

// SnapshotConfig configures the headless visual simulator.
type SnapshotConfig struct {
	Dir      string
	Interval time.Duration
	RingSize int
}

// NewHeadless builds the development driver.
func NewHeadless(log *slog.Logger, width, height int) *Headless {
	return NewHeadlessWithSnapshots(log, width, height, SnapshotConfig{})
}

// NewHeadlessWithSnapshots builds the driver with the visual-simulator
// options (empty dir disables snapshots).
func NewHeadlessWithSnapshots(log *slog.Logger, width, height int, sc SnapshotConfig) *Headless {
	if width <= 0 {
		width = 1280
	}
	if height <= 0 {
		height = 800
	}
	if sc.Interval <= 0 {
		sc.Interval = 10 * time.Second
	}
	if sc.RingSize <= 0 || sc.RingSize > ringSize {
		sc.RingSize = ringSize
	}
	if sc.Dir != "" {
		if err := os.MkdirAll(sc.Dir, 0o755); err != nil {
			// A bad snapshot dir must not take the frame down: log
			// and continue snapshotless.
			log.Error("snapshot dir unavailable; continuing without snapshots",
				"dir", sc.Dir, "error", err)
			sc.Dir = ""
		}
	}
	return &Headless{
		log:          log,
		snapDir:      sc.Dir,
		snapInterval: sc.Interval,
		ringSize:     sc.RingSize,
		width:        width,
		height:       height,
		brightness:   0.8,
		power:        true,
	}
}

func (h *Headless) Name() string { return "headless" }

func (h *Headless) Info() Info {
	h.mu.RLock()
	defer h.mu.RUnlock()
	extra := ""
	if h.snapDir != "" {
		extra = "snapshots: " + h.snapDir
	}
	return Info{
		Name:               "headless",
		ResolutionWidth:    h.width,
		ResolutionHeight:   h.height,
		SupportsBrightness: false,
		SupportsPower:      false,
		Extra:              extra,
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

// FrameSize reports the driver's expected frame dimensions.
func (h *Headless) FrameSize() (width, height int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.width, h.height
}

func (h *Headless) PresentFrame(ctx context.Context, img *image.NRGBA) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.initialized {
		return fmt.Errorf("headless driver: not initialized")
	}
	if img.Bounds() != image.Rect(0, 0, h.width, h.height) {
		return fmt.Errorf("headless driver: frame %v != expected %dx%d",
			img.Bounds(), h.width, h.height)
	}
	// Copy: the UI reuses its canvas right after returning.
	cp := image.NewNRGBA(img.Rect)
	copy(cp.Pix, img.Pix)
	h.last = cp
	h.presents++
	if h.snapDir != "" {
		h.maybeSnapshotLocked(ctx, cp)
	}
	h.log.DebugContext(ctx, "headless: frame presented", "n", h.presents)
	return nil
}

// maybeSnapshotLocked writes latest.png (always) and a timestamped
// ring entry (rate-limited). Caller holds h.mu.
func (h *Headless) maybeSnapshotLocked(ctx context.Context, img *image.NRGBA) {
	now := time.Now()
	if now.Sub(h.lastSnapAt) >= h.snapInterval || h.lastSnapAt.IsZero() {
		h.lastSnapAt = now
		tmp := filepath.Join(h.snapDir, "latest.png.tmp")
		if err := encodePNG(tmp, img); err != nil {
			h.log.WarnContext(ctx, "snapshot write failed", "error", err)
			return
		}
		if err := os.Rename(tmp, filepath.Join(h.snapDir, "latest.png")); err != nil {
			h.log.WarnContext(ctx, "snapshot rename failed", "error", err)
			return
		}
		// Timestamped ring entry (bounded).
		idx := int(now.UnixNano() % int64(h.ringSize))
		name := fmt.Sprintf("frame-%02d-%s.png", idx, now.Format("150405.000"))
		if err := encodePNG(filepath.Join(h.snapDir, name), img); err == nil {
			h.ringName[idx] = name
		}
	}
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
	// Fallback behaviour (spec §11): with no hardware backlight the
	// documented degradation is a black frame.
	if !on && h.last != nil {
		h.log.DebugContext(ctx, "headless: display power off (black frame fallback)")
	}
	h.log.DebugContext(ctx, "headless: display power", "on", on)
	return nil
}

func (h *Headless) SetRotation(ctx context.Context, degrees int) error {
	switch degrees {
	case 0, 90, 180, 270:
	default:
		return fmt.Errorf("rotation %d invalid (0|90|180|270)", degrees)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rotation = degrees
	return nil
}

// --- test / doctor helpers ------------------------------------------

// LastFrame returns a copy of the most recently presented frame (nil
// until the first PresentFrame).
func (h *Headless) LastFrame() *image.NRGBA {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.last == nil {
		return nil
	}
	cp := image.NewNRGBA(h.last.Rect)
	copy(cp.Pix, h.last.Pix)
	return cp
}

// LastFramePixel returns the pixel at (x,y) of the last frame — a cheap
// assertion point for UI tests (avoids copying a full frame).
func (h *Headless) LastFramePixel(x, y int) (color.NRGBA, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.last == nil {
		return color.NRGBA{}, fmt.Errorf("no frame presented yet")
	}
	if x < 0 || y < 0 || x >= h.width || y >= h.height {
		return color.NRGBA{}, fmt.Errorf("pixel (%d,%d) out of bounds", x, y)
	}
	return h.last.At(x, y).(color.NRGBA), nil
}

// Presents returns how many frames have been presented.
func (h *Headless) Presents() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.presents
}

// Brightness / Power / Rotation report stored state.
func (h *Headless) Brightness() float64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.brightness
}

func (h *Headless) Power() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.power
}

func (h *Headless) Rotation() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rotation
}

// encodePNG writes img to path atomically (temp + rename).
func encodePNG(path string, img *image.NRGBA) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = png.Encode(f, img)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
