// Package ui is the frame's native touch interface (spec §10):
// slideshow, gallery, albums, search, favourites, settings and the
// sleep screen, drawn with the 2-D canvas primitives and driven by
// touch gestures.
//
// Design (spec §10 accessibility):
//   - large text and touch targets (font scale 2/3, buttons >= 72px)
//   - high contrast (white/grey on black)
//   - predictable gestures: swipe = navigate, tap = select/show
//     controls, double-tap = photo details, hold = confirm
//     (administrative actions are hold-protected)
//   - minimal chrome, no unnecessary animation (the only animation is
//     the configurable photograph transition)
//   - clear empty/error states (unpaired, waiting for first sync,
//     last sync error)
//
// The Engine is synchronous and deterministic: Run() drives it with a
// real clock and the gesture channel, while tests drive it directly
// via Tick/HandleGesture with an injected clock.
package ui

import (
	"context"
	"image"
	"log/slog"
	"sync"
	"time"

	"github.com/matthewgall/openframe/internal/frame/canvas"
	"github.com/matthewgall/openframe/internal/frame/display"
	"github.com/matthewgall/openframe/internal/frame/touch"
)

// Screen identifies a UI screen.
type Screen int

const (
	ScreenSlideshow Screen = iota
	ScreenDetails
	ScreenGallery
	ScreenAlbum
	ScreenSearch
	ScreenSettings
	ScreenSleep
)

func (s Screen) String() string {
	switch s {
	case ScreenSlideshow:
		return "slideshow"
	case ScreenDetails:
		return "details"
	case ScreenGallery:
		return "gallery"
	case ScreenAlbum:
		return "album"
	case ScreenSearch:
		return "search"
	case ScreenSettings:
		return "settings"
	case ScreenSleep:
		return "sleep"
	}
	return "unknown"
}

// Photo is one active-collection photograph as the UI sees it.
type Photo struct {
	Hash        string
	Name        string
	Caption     string
	AlbumID     string
	AlbumName   string
	Uploader    string
	CaptureDate time.Time // zero if unknown
	DisplayPath string
	ThumbPath   string
	Priority    bool
	Favourite   bool
}

// Album is one active-collection album.
type Album struct {
	ID        string
	Name      string
	CoverHash string
	Count     int
}

// Info is frame-level state shown on the settings screen.
type Info struct {
	DeviceName      string
	ServerURL       string
	ServerReachable bool
	LastSync        *time.Time
	LastSyncError   string
	FreeBytes       int64
	Version         string
	Paired          bool
}

// Data is the local state the UI reads and writes (implemented by the
// frame package over frame.db + the sync engine).
type Data interface {
	// Collection returns the ACTIVE collection (albums + photos).
	Collection(ctx context.Context) ([]Album, []Photo, error)
	// Favourite reports the local favourite state (usable offline).
	Favourite(ctx context.Context, hash string) bool
	// SetFavourite records a local favourite change (offline-queued).
	SetFavourite(ctx context.Context, hash string, value bool) error
	// Info reports frame-level state.
	Info(ctx context.Context) Info
}

// Config carries the UI-relevant slideshow settings.
type Config struct {
	Interval           time.Duration
	Order              string // sequential | random
	Fit                canvas.FitMode
	ShowCaption        bool
	ShowClock          bool
	Transition         string // none | crossfade | slide
	TransitionDuration time.Duration
	// Device-level values the settings screen shows and stages:
	Brightness  float64
	Orientation int // 0|90|180|270
	SleepOn     int // minutes from midnight
	SleepOff    int
	// Now is the clock source (injected for tests).
	Now func() time.Time
	// ApplySettings persists staged settings (hold-protected) and
	// applies them to the driver/config. Nil disables applying (tests).
	ApplySettings func(s Settings) error
}

// Engine is the UI state machine.
type Engine struct {
	// mu guards all engine state. The Run goroutine owns the UI;
	// Reload/SetAsleep/SetThrottled arrive from other components and
	// take the lock briefly. Render is always under the lock.
	mu   sync.Mutex
	cv   *canvas.Canvas
	drv  display.Driver
	data Data
	cfg  *Config
	log  *slog.Logger
	in   <-chan touch.Gesture

	w, h int

	// state
	screen  Screen
	all     []Photo // active collection, canonical order
	albums  []Album
	list    []Photo // current slideshow list
	listKey string  // "all" | "favourites" | albumID | "search"
	idx     int

	// navigation state
	galleryTop int
	albumTop   int
	searchTop  int
	sortByName bool
	filterChar rune // 0 = no filter

	// slideshow state
	controls     bool
	lastActivity time.Time
	curImage     *image.NRGBA // fitted current photograph (nil = none)

	// transition state (only valid while animating)
	transFrom *image.NRGBA
	transTo   *image.NRGBA

	// sleep
	asleep bool

	// thermal throttle
	throttled bool

	// M5 pairing: the code shown on this screen while the frame waits
	// for admin approval ("" = no active challenge).
	pairingCode string

	// last present failure (M5 health summary; "" when healthy).
	lastPresentErr string

	// settings staging
	staged Settings

	// slideshow advance scheduling (Run owns the tickers; Tick may
	// reset the advance ticker when the effective interval changes —
	// thermal throttle or a settings apply).
	advanceTick  *time.Ticker
	lastInterval time.Duration

	// hit targets for the last rendered screen
	buttons []hitButton
}

type hitButton struct {
	label string
	rect  [4]int // x0, y0, x1, y1
	kind  int
}

// Button kinds (semantic actions; labels are rendered text).
const (
	btnPrev = iota
	btnNext
	btnStar
	btnInfo
	btnGallery
	btnSlideshow
	btnFavourites
	btnAlbum
	btnSearch
	btnSettings
	btnBack
	btnApply
	btnSort
	browsePhoto // grid/row tap -> open photo
	btnSetting  // followed by a setting index (btnSetting + 0..7);
	// MUST stay last: doButton range-checks btnSetting..btnSetting+7.
)

// New builds an Engine. The display must already be inited; Run
// presents the first frame immediately.
func New(cv *canvas.Canvas, drv display.Driver, data Data, cfg *Config, log *slog.Logger, in <-chan touch.Gesture) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	w, h := drv.FrameSize()
	e := &Engine{
		cv: cv, drv: drv, data: data, cfg: cfg,
		log:     log,
		in:      in,
		w:       w,
		h:       h,
		staged:  SettingsFromConfig(cfg),
		listKey: "all",
	}
	return e
}

// Run drives the UI until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	e.Reload(ctx)
	e.mu.Lock()
	e.render()
	e.lastActivity = e.cfg.Now()
	e.lastInterval = e.effectiveInterval()
	e.advanceTick = time.NewTicker(e.lastInterval)
	e.mu.Unlock()
	defer e.advanceTick.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case g := <-e.in:
			e.HandleGesture(g)
		case <-tick.C:
			e.Tick(e.cfg.Now())
		case <-e.advanceTick.C:
			e.mu.Lock()
			if e.shouldAdvance() {
				e.advance(1)
			}
			e.mu.Unlock()
		}
	}
}

// shouldAdvance reports whether the slideshow timer should advance
// now (awake, on the slideshow screen, with photos to show).
func (e *Engine) shouldAdvance() bool {
	return !e.asleep && e.screen == ScreenSlideshow && len(e.list) > 0
}

// effectiveInterval applies the thermal throttle (spec §7: throttle
// by reducing sustained load).
func (e *Engine) effectiveInterval() time.Duration {
	d := e.cfg.Interval
	if e.throttled {
		d *= 2
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}

// Tick handles 1 Hz housekeeping: auto-hiding controls, the 120s
// gallery inactivity timeout (spec §10) and clock redraws.
func (e *Engine) Tick(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	if want := e.effectiveInterval(); want != e.lastInterval && e.advanceTick != nil {
		e.lastInterval = want
		e.advanceTick.Reset(want)
	}
	if e.controls && now.Sub(e.lastActivity) > 5*time.Second {
		e.controls = false
		changed = true
	}
	if e.screen != ScreenSlideshow && e.screen != ScreenSleep &&
		now.Sub(e.lastActivity) > 120*time.Second {
		e.goSlideshow("all", -1)
		changed = true
	}
	if e.screen == ScreenSlideshow && e.cfg.ShowClock {
		// Redraw at most once a second for the clock.
		changed = true
	}
	if changed {
		e.render()
	}
}

// SetThrottled is called by the thermal monitor; the slideshow interval
// doubles while true.
func (e *Engine) SetThrottled(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.throttled == on {
		return
	}
	e.throttled = on
	e.log.Info("thermal throttle", "on", on)
}

// SetAsleep is called by the sleep scheduler. The agent keeps running;
// only the displayed content changes (the driver's power state is the
// scheduler's job).
func (e *Engine) SetAsleep(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.asleep == on {
		return
	}
	e.asleep = on
	e.screen = ScreenSleep
	if !on {
		e.screen = ScreenSlideshow
	}
	e.render()
}

// LastPresentError returns the most recent display present failure
// summary ("" when the last present succeeded). Used by the M5 health
// reporter.
func (e *Engine) LastPresentError() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastPresentErr
}

// SetPairing publishes (or clears, with "") the pairing code shown on
// the not-yet-paired screen (M5, spec §12). When a code is present the
// empty state becomes a pairing screen.
func (e *Engine) SetPairing(code string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pairingCode == code {
		return
	}
	e.pairingCode = code
	e.log.Info("pairing code published", "present", code != "")
	e.render()
}

// Reload re-reads the active collection (called at startup and after
// each sync activation). It preserves the user's position as far as
// possible.
func (e *Engine) Reload(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	albums, photos, err := e.data.Collection(ctx)
	if err != nil {
		e.log.Warn("collection read failed; keeping previous", "error", err)
		return
	}
	e.albums = albums
	e.all = photos
	// Re-derive the current list.
	if e.listKey == "all" || e.listKey == "" {
		e.listKey = "all"
	}
	e.list = e.listForKey(e.listKey)
	if e.idx >= len(e.list) {
		e.idx = 0
	}
	e.curImage = nil
}

// listForKey builds a slideshow list for a key.
func (e *Engine) listForKey(key string) []Photo {
	var out []Photo
	switch key {
	case "all":
		out = e.all
	case "favourites":
		for _, p := range e.all {
			if p.Favourite {
				out = append(out, p)
			}
		}
	case "search":
		out = e.searchList()
	default: // album id
		for _, p := range e.all {
			if p.AlbumID == key {
				out = append(out, p)
			}
		}
	}
	return out
}

// searchList returns the search screen list (sorted, letter-filtered).
func (e *Engine) searchList() []Photo {
	out := make([]Photo, 0, len(e.all))
	for _, p := range e.all {
		if e.filterChar != 0 && !matchesChar(p, e.filterChar) {
			continue
		}
		out = append(out, p)
	}
	if e.sortByName {
		sortPhotosByName(out)
	} else {
		sortPhotosByDate(out)
	}
	return out
}

// Screen reports the current screen.
func (e *Engine) Screen() Screen {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.screen
}

// Config exposes the live config (the settings screen applies staged
// values into it; the frame persists the change). Cross-goroutine
// updates must go through ApplyExternalConfig.
func (e *Engine) Config() *Config { return e.cfg }

// ExternalConfig is the subset of the live config the M5 effective-
// config applier manages (server config + local settings + config
// file). Fields the applier does not manage (order, fit, transition
// duration, callbacks) are left untouched.
type ExternalConfig struct {
	Interval    time.Duration
	Transition  string
	ShowCaption bool
	ShowClock   bool
	Brightness  float64
	Orientation int
	SleepOn     int // minutes from midnight
	SleepOff    int // minutes from midnight
}

// ApplyExternalConfig updates the live settings from another
// goroutine (the M5 effective-config applier). It copies values under
// the lock; the Run loop's Tick re-arms the advance ticker when the
// effective interval changed.
func (e *Engine) ApplyExternalConfig(c ExternalConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	intervalChanged := e.cfg.Interval != c.Interval
	e.cfg.Interval = c.Interval
	e.cfg.Transition = c.Transition
	e.cfg.ShowCaption = c.ShowCaption
	e.cfg.ShowClock = c.ShowClock
	e.cfg.Brightness = c.Brightness
	e.cfg.Orientation = c.Orientation
	e.cfg.SleepOn = c.SleepOn
	e.cfg.SleepOff = c.SleepOff
	if intervalChanged {
		e.lastInterval = 0 // force Tick to re-derive and re-arm
	}
	e.log.Info("external config applied",
		"interval", e.cfg.Interval.String(),
		"transition", e.cfg.Transition,
		"brightness", e.cfg.Brightness)
}

// CurrentPhoto reports the photo the slideshow is showing (nil when
// none, e.g. an empty collection).
func (e *Engine) CurrentPhoto() *Photo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentPhotoLocked()
}

// currentPhotoLocked is the render/gesture path; caller holds e.mu.
func (e *Engine) currentPhotoLocked() *Photo {
	if e.idx < 0 || e.idx >= len(e.list) {
		return nil
	}
	p := e.list[e.idx]
	return &p
}

// Present forces a present of the current canvas (test helper).
func (e *Engine) Present() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.present()
}

func (e *Engine) present() {
	// The canvas is exactly the driver size; copy happens in the
	// driver. A failed present must not kill the UI: the last image
	// stays on the panel. Note: called with e.mu held (all present
	// call sites run under the lock) — no lock here.
	if err := e.drv.PresentFrame(context.Background(), e.cv.Image()); err != nil {
		e.lastPresentErr = err.Error()
		e.log.Warn("present failed (display keeps last image)", "error", err)
		return
	}
	e.lastPresentErr = ""
}
