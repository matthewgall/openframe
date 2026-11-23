package frame

// Milestone 4 display wiring: the UI engine (slideshow, gallery,
// search, settings), touch input, sleep scheduling and thermal
// throttling, all reading from the ACTIVE collection so the display
// path stays offline-first (ADR-0008).
//
// Failure isolation (spec §7): the UI component owns pixels; the sync
// engine owns network and staged/active state. A present failure
// keeps the last frame on screen (the driver holds it); a sync
// failure keeps the collection displayed. Each reports through its
// own component in the bounded runner.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/frame/canvas"
	"github.com/matthewgall/openframe/internal/frame/sleep"
	"github.com/matthewgall/openframe/internal/frame/thermal"
	"github.com/matthewgall/openframe/internal/frame/touch"
	"github.com/matthewgall/openframe/internal/frame/ui"
	"github.com/matthewgall/openframe/internal/platform"
	"github.com/matthewgall/openframe/internal/version"
	wifipkg "github.com/matthewgall/openframe/internal/wifi"
)

// gestureChannelBuffer bounds the input channel so a stalled UI can
// never grow memory; dropped gestures are correct (the UI polls the
// active state, gestures are transient).
const gestureChannelBuffer = 32

// uiState bundles the M4 display pieces so components can share them
// without growing the Frame struct further.
type uiState struct {
	eng       *ui.Engine
	data      *frameData
	mon       *thermal.Monitor
	gestures  chan touch.Gesture
	throttled bool

	// schedMu guards sched (written by the settings apply path on the
	// UI goroutine, read once a second by the sleep component).
	schedMu sync.Mutex
	sched   sleep.Schedule
}

func (s *uiState) schedule() sleep.Schedule {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.sched
}

func (s *uiState) setSchedule(sc sleep.Schedule) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	s.sched = sc
}

// buildDisplay assembles the UI stack. It must run after the display
// driver is initialised (it needs the driver's frame size).
func (f *Frame) buildDisplay(ctx context.Context) (*uiState, error) {
	w, h := f.drv.FrameSize()
	cv := canvas.New(w, h)

	// Local settings override the config file for this frame.
	settings := LoadLocalSettings(ctx, f.db)
	if err := applyLocalSettings(&f.cfg.Frame, settings); err != nil {
		return nil, fmt.Errorf("apply local settings: %w", err)
	}
	f.applyStoredNumericSettings(settings)

	fit := canvas.FitContain
	switch f.cfg.Frame.Slideshow.Fit {
	case "cover":
		fit = canvas.FitCover
	case "crop":
		fit = canvas.FitCrop
	}

	uiCfg := &ui.Config{
		Interval:           f.cfg.Frame.Slideshow.Interval,
		Order:              f.cfg.Frame.Slideshow.Order,
		Fit:                fit,
		ShowCaption:        f.cfg.Frame.Slideshow.ShowCaption,
		ShowClock:          f.cfg.Frame.Slideshow.ShowClock,
		Transition:         f.cfg.Frame.Slideshow.Transition,
		TransitionDuration: f.cfg.Frame.Slideshow.TransitionDuration,
		Brightness:         f.cfg.Frame.Display.Brightness,
		Orientation:        f.orientationDegrees(),
		SleepOn:            hmToMinutes(f.cfg.Frame.Sleep.On),
		SleepOff:           hmToMinutes(f.cfg.Frame.Sleep.Off),
		Now:                time.Now,
		ApplySettings:      f.applyUIState,
	}

	data := &frameData{f: f}
	gestures := make(chan touch.Gesture, gestureChannelBuffer)
	eng := ui.New(cv, f.drv, data, uiCfg, f.log, gestures)

	// Wi-Fi setup (ADR-0014): install the backend — the UI hides the
	// whole feature when the platform has no Wi-Fi management — and
	// resume saved networks in the background so a powered-off frame
	// rejoins its network on its own.
	eng.SetWifi(wifipkg.New())
	go func() {
		ctx2, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := eng.WifiAutoConnect(ctx2); err != nil {
			f.log.Info("wifi autoconnect: nothing to resume", "error", err)
		}
	}()

	sched, err := sleep.FromConfig(f.cfg.Frame.Sleep.On, f.cfg.Frame.Sleep.Off, f.cfg.Frame.Sleep.TZ)
	if err != nil {
		return nil, fmt.Errorf("sleep schedule: %w", err)
	}
	mon := thermal.NewMonitor(f.cfg.Frame.Thermal.Enabled, thermal.NewSysfsReader("/sys/class/thermal"), f.cfg.Frame.Thermal.ThresholdC)

	st := &uiState{eng: eng, data: data, sched: sched, mon: mon, gestures: gestures}

	// Apply the stored display values to the driver up front.
	if err := f.drv.SetBrightness(ctx, f.cfg.Frame.Display.Brightness); err != nil {
		f.log.Warn("initial brightness failed", "error", err)
	}
	if f.orientationDegrees() != 0 {
		if err := f.drv.SetRotation(ctx, f.orientationDegrees()); err != nil {
			f.log.Warn("initial rotation failed", "error", err)
		}
	}

	// Sync activations refresh the UI (and only the UI): the active
	// collection changed atomically, the engine re-reads it. The
	// arrival list (hashes new vs the previous active set) seeds the
	// debut queue (U7); until that lands the count is logged so the
	// signal is observable in the field.
	f.engine.OnActivate(func(revision int64, arrivals []string) {
		eng.Reload(ctx)
		if len(arrivals) > 0 {
			f.log.Info("photos arrived", "revision", revision, "count", len(arrivals))
		}
		f.log.Info("display refreshed after activation", "revision", revision)
	})

	f.ui = st
	return st, nil
}

// orientationDegrees maps the config orientation string to degrees.
// "auto" resolves to 0 (landscape) — the frame mounts landscape by
// default; hardware auto-detection of panel orientation is not
// exposed by DRM and would be a guess.
func (f *Frame) orientationDegrees() int {
	switch f.cfg.Frame.Display.Orientation {
	case "90":
		return 90
	case "180":
		return 180
	case "270":
		return 270
	default:
		return 0
	}
}

// applyStoredNumericSettings loads persisted overrides into the UI
// config values (minutes for sleep, durations, booleans).
func (f *Frame) applyStoredNumericSettings(s map[string]string) {
	// Values were already merged into f.cfg by applyLocalSettings;
	// this hook exists for values that do not map onto config fields.
	_ = s
}

// applyUIState persists staged settings (from a hold on the settings
// screen) and applies them live.
func (f *Frame) applyUIState(st ui.Settings) error {
	ctx := context.Background()
	var errs []error
	save := func(key, val string) {
		if err := SaveLocalSetting(ctx, f.db, key, val); err != nil {
			errs = append(errs, err)
		}
	}
	save(settingBrightness, fmt.Sprintf("%g", st.Brightness))
	save(settingSlideshowInterval, st.SlideshowInterval.String())
	save(settingTransition, st.Transition)
	save(settingOrientation, fmt.Sprintf("%d", st.Orientation))
	// Sleep times persist in the SAME HH:MM form the config file and
	// the scheduler use. Saving minutes here crashed the next boot
	// ("sleep schedule: want HH:MM") and every settings apply
	// silently poisoned frame.db until then.
	save(settingSleepOn, minutesToHM(st.SleepOn))
	save(settingSleepOff, minutesToHM(st.SleepOff))
	save(settingShowCaption, fmt.Sprintf("%t", st.ShowCaption))
	save(settingShowClock, fmt.Sprintf("%t", st.ShowClock))

	// Live-apply to the running UI and driver.
	cfg := f.ui.eng.Config()
	cfg.Interval = st.SlideshowInterval
	cfg.Transition = st.Transition
	cfg.ShowCaption = st.ShowCaption
	cfg.ShowClock = st.ShowClock
	cfg.Brightness = st.Brightness
	cfg.Orientation = st.Orientation
	cfg.SleepOn = st.SleepOn
	cfg.SleepOff = st.SleepOff
	if err := f.drv.SetBrightness(ctx, st.Brightness); err != nil {
		f.log.Warn("brightness apply failed", "error", err)
	}
	if err := f.drv.SetRotation(ctx, st.Orientation); err != nil {
		f.log.Warn("rotation apply failed", "error", err)
	}
	// Rebuild the sleep schedule.
	if sched, err := sleep.FromConfig(minutesToHM(st.SleepOn), minutesToHM(st.SleepOff), f.effectiveConfig().Frame.Sleep.TZ); err == nil {
		f.ui.setSchedule(sched)
	}
	if len(errs) > 0 {
		return fmt.Errorf("persist settings: %d failed: %w", len(errs), errs[0])
	}
	f.log.Info("settings applied", "brightness", st.Brightness, "interval", st.SlideshowInterval, "orientation", st.Orientation)
	return nil
}

func minutesToHM(m int) string {
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

func hmToMinutes(s string) int {
	h, m, ok := parseHMSimple(s)
	if !ok {
		return 0
	}
	return h*60 + m
}

func parseHMSimple(s string) (int, int, bool) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// --- components --------------------------------------------------------

// uiComponent runs the UI engine loop. It is the main display path:
// it blocks until ctx is cancelled and presents every frame.
func (f *Frame) uiComponent(ctx context.Context) error {
	if f.ui == nil {
		return fmt.Errorf("display not initialised")
	}
	return f.ui.eng.Run(ctx)
}

// touchComponent feeds the gesture recognizer (real or dev input)
// into the UI input channel. Production input is the evdev node;
// frame.touch.dev_input points at a FIFO of synthetic finger samples
// for headless UI testing (see touch.DevInput and docs/development.md).
func (f *Frame) touchComponent(ctx context.Context) error {
	eff := f.effectiveConfig()
	if !eff.Frame.Touch.Enabled {
		return nil
	}

	var samples <-chan touch.Sample
	if eff.Frame.Touch.DevInput != "" {
		di, err := touch.DevInputFromFile(eff.Frame.Touch.DevInput, f.log)
		if err != nil {
			f.log.Warn("dev input unavailable; running touchless", "path", eff.Frame.Touch.DevInput, "error", err)
			return nil
		}
		f.log.Info("dev input source attached", "path", eff.Frame.Touch.DevInput)
		samples = di.Stream(ctx)
	} else {
		devPath := eff.Frame.Touch.Device
		if devPath == "" {
			p, err := touch.FindTouchscreen("", "")
			if err != nil {
				f.log.Info("no touchscreen found; running touchless", "error", err)
				return nil
			}
			devPath = p
		}
		dev, err := touch.Open(devPath)
		if err != nil {
			f.log.Warn("touch open failed; running touchless", "device", devPath, "error", err)
			return nil
		}
		defer dev.Close()
		f.log.Info("touch device attached", "device", devPath)
		// Raw HID axes (e.g. 0..4096) must land in panel pixels for the
		// gesture hit-testing to work at any resolution.
		dev.SetDisplaySize(f.drv.FrameSize())
		samples = dev.Stream(ctx)
	}

	rec := touch.NewRecognizer(touch.Options{})
	for s := range samples {
		for _, g := range rec.Feed(s) {
			select {
			case f.ui.gestures <- g:
			default: // UI is backed up: drop (transient input)
			}
		}
	}
	return nil
}

// touchWakeGrace is how long a recent touch holds off scheduled sleep:
// a tap wakes the frame (see ui.Engine gesture handling), and the
// recipient should get a little time to browse before the panel dozes
// again.
const touchWakeGrace = 60 * time.Second

// sleepComponent evaluates the daily sleep schedule once a second:
// asleep turns the display off (backlight or black frame) while the
// frame keeps running and syncing (spec §11). A frame the recipient
// just woke by touch stays awake for touchWakeGrace.
func (f *Frame) sleepComponent(ctx context.Context) error {
	eff := f.effectiveConfig()
	if eff.Frame.Sleep.On == "" && eff.Frame.Sleep.Off == "" {
		return nil
	}
	f.log.Info("sleep schedule active", "schedule", f.ui.schedule().Description())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			asleep := f.ui.schedule().IsAsleep(now)
			// An unpaired frame never sleeps: the pairing/setup screen
			// is the only thing on display, and the owner pairing it
			// may well be standing in front of it at night. Hiding the
			// code behind the sleep schedule made pairing impossible
			// outside the wake window.
			if !f.engine.Paired() {
				asleep = false
			}
			// Touch wins briefly over the schedule.
			if asleep && f.ui.eng.RecentlyActive(touchWakeGrace) {
				asleep = false
			}
			if asleep == f.ui.eng.Asleep() {
				continue
			}
			f.ui.eng.SetAsleep(asleep) // also drives display power
			if asleep {
				f.log.Info("frame sleeping (display off)")
			} else {
				f.log.Info("frame awake (display on)")
			}
		}
	}
}

// connectivityComponent keeps the clock's link glyph current: it
// polls the kernel's interface state (cheap sysfs reads) and server
// reachability (one HTTP probe) every 10 s and pushes the pair to
// the engine. Reachability is polled here rather than in Info()
// because Info() runs on the UI goroutine and a clock redraw must
// not wait on a 2 s HTTP timeout.
func (f *Frame) connectivityComponent(ctx context.Context) error {
	if f.ui == nil {
		return nil
	}
	syncLink := func() {
		var ls ui.LinkState
		switch platform.Connectivity() {
		case platform.LinkWired:
			ls = ui.LinkWired
		case platform.LinkWiFi:
			ls = ui.LinkWiFi
		}
		f.ui.eng.SetLink(ls, f.serverReachable(ctx))
	}
	syncLink()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			syncLink()
		}
	}
}

// thermalComponent polls the temperature monitor and doubles the
// slideshow interval above the threshold (spec §13).
func (f *Frame) thermalComponent(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			throttled := f.ui.mon.Throttled()
			if throttled == f.ui.throttled {
				continue
			}
			f.ui.throttled = throttled
			f.ui.eng.SetThrottled(throttled)
			if throttled {
				f.log.Warn("thermal throttle: slideshow interval doubled", "threshold_c", f.effectiveConfig().Frame.Thermal.ThresholdC)
			} else {
				f.log.Info("thermal throttle released")
			}
		}
	}
}

// --- ui.Data implementation --------------------------------------------

// frameData serves the UI from the frame database and cache: the
// ACTIVE collection only (never staged), so the display can never
// show a half-synced set.
type frameData struct {
	f *Frame
}

func (d *frameData) Collection(ctx context.Context) ([]ui.Album, []ui.Photo, error) {
	f := d.f
	albums, err := d.albums(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := f.db.QueryContext(ctx,
		`SELECT hash, name, caption, album_id, capture_date, priority, uploader
		   FROM photos
		  ORDER BY
		    CASE WHEN capture_date IS NULL OR capture_date = '' THEN 1 ELSE 0 END,
		    capture_date DESC, hash`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type row struct {
		hash, name, caption, albumID string
		capture                      string
		uploader                     string
		priority                     bool
	}
	var rs []row
	for rows.Next() {
		var r row
		var cap sql.NullString
		if err := rows.Scan(&r.hash, &r.name, &r.caption, &r.albumID, &cap, &r.priority, &r.uploader); err != nil {
			return nil, nil, err
		}
		r.capture = cap.String
		rs = append(rs, r)
	}
	if rows.Err() != nil {
		return nil, nil, rows.Err()
	}

	albumName := map[string]string{}
	for _, a := range albums {
		albumName[a.ID] = a.Name
	}

	photos := make([]ui.Photo, 0, len(rs))
	for _, r := range rs {
		p := ui.Photo{
			Hash:      r.hash,
			Name:      r.name,
			Caption:   r.caption,
			AlbumID:   r.albumID,
			AlbumName: albumName[r.albumID],
			Uploader:  r.uploader,
			Priority:  r.priority,
		}
		if r.capture != "" {
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
				if t, err := time.Parse(layout, r.capture); err == nil {
					p.CaptureDate = t
					break
				}
			}
		}
		p.DisplayPath = f.cache.Path(r.hash, "display")
		p.ThumbPath = f.cache.Path(r.hash, "thumb")
		if !f.cache.Exists(r.hash, "display") {
			// The file is still downloading: skip it (the UI shows
			// what is actually on disk; activation only completes
			// when every mandatory file is verified, so this window
			// is short and bounded).
			continue
		}
		p.Favourite = d.Favourite(ctx, r.hash)
		photos = append(photos, p)
	}
	return albums, photos, nil
}

func (d *frameData) albums(ctx context.Context) ([]ui.Album, error) {
	rows, err := d.f.db.QueryContext(ctx,
		`SELECT a.id, a.name, a.cover_hash,
		        (SELECT COUNT(*) FROM photos p WHERE p.album_id = a.id)
		   FROM albums a
		  ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ui.Album
	for rows.Next() {
		var a ui.Album
		var cover sql.NullString
		if err := rows.Scan(&a.ID, &a.Name, &cover, &a.Count); err != nil {
			return nil, err
		}
		a.CoverHash = cover.String
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *frameData) Favourite(ctx context.Context, hash string) bool {
	var v int
	err := d.f.db.QueryRowContext(ctx,
		"SELECT value FROM favourites WHERE photo_hash = ?", hash).Scan(&v)
	if err != nil {
		return false
	}
	return v == 1
}

func (d *frameData) SetFavourite(ctx context.Context, hash string, value bool) error {
	// Offline-first: the op is queued locally and replayed to the
	// server when connectivity returns (M3 changes engine).
	if d.f.engine == nil {
		return fmt.Errorf("not paired")
	}
	return d.f.engine.SetFavourite(ctx, hash, value)
}

func (d *frameData) Info(ctx context.Context) ui.Info {
	f := d.f
	eff := f.effectiveConfig()
	info := ui.Info{
		DeviceName:      f.deviceName(ctx),
		ServerURL:       eff.Frame.ServerURL,
		ServerReachable: f.serverReachable(ctx),
		Version:         version.Version,
	}
	// "Last sync" means "last successful manifest check" - the same
	// event the admin UI reports (health.last_sync_at comes from
	// LastAttempt). It used to show keyLastSuccess (collection
	// ACTIVATION), which stays days old on a healthy, unchanged
	// library and made a perfectly synced frame look stale - and
	// LastSyncError was never populated, leaving the slideshow's
	// "Last sync error" line dead code.
	if f.engine != nil {
		if t, ok := f.engine.LastAttempt(ctx); ok {
			tt := t
			info.LastSync = &tt
			info.LastSyncError = f.engine.LastError(ctx)
		}
		if v, ok := diskFree(eff); ok {
			info.FreeBytes = v
		}
	} else {
		// Headless/test path without a sync engine: fall back to the
		// stored activation timestamp (previous behaviour).
		info.LastSync = f.lastSyncTime(ctx)
	}
	info.Paired = f.engine != nil && f.engine.Paired()
	return info
}

func (f *Frame) deviceName(ctx context.Context) string {
	// M3 stores the server-assigned name in the sync_state table (the
	// sync engine's own key/value store). Table names here were
	// silently wrong ("sync"/"agent") for the life of the feature:
	// SQLite rejects them, the read fails, and every consumer fell
	// back to defaults with no error surfaced.
	if v, err := f.stateValue(ctx, "sync_state", "device_name"); err == nil && v != "" {
		return v
	}
	if v, err := f.stateValue(ctx, "agent_state", StateKeyDeviceName); err == nil && v != "" {
		return v
	}
	return "Frame"
}

func (f *Frame) stateValue(ctx context.Context, table, key string) (string, error) {
	var v string
	err := f.db.QueryRowContext(ctx,
		"SELECT value FROM "+table+" WHERE key = ?", key).Scan(&v)
	return v, err
}

func (f *Frame) serverReachable(ctx context.Context) bool {
	serverURL := f.effectiveConfig().Frame.ServerURL
	if serverURL == "" {
		return false
	}
	return platform.Reachable(ctx, serverURL+"/healthz", 2*time.Second) == nil
}

func (f *Frame) lastSyncTime(ctx context.Context) *time.Time {
	if f.engine != nil {
		if t, ok := f.engine.LastSync(ctx); ok {
			return &t
		}
		return nil
	}
	v, err := f.stateValue(ctx, "sync_state", "last_success")
	if err != nil || v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil
	}
	return &t
}

// diskFree is the frame data partition's free space (the same statfs
// the health reporter uses). ok=false when the path is unreadable.
func diskFree(cfg config.Config) (int64, bool) {
	_, frameDir, _ := cfg.Paths()
	d, err := platform.Disk(frameDir)
	if err != nil {
		return 0, false
	}
	return d.FreeBytes, true
}
