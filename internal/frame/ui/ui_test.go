package ui

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/frame/canvas"
	"github.com/matthewgall/openframe/internal/frame/display"
	"github.com/matthewgall/openframe/internal/frame/touch"
)

// --- test harness ----------------------------------------------------

type fakeData struct {
	albums  []Album
	photos  []Photo
	favs    map[string]bool
	info    Info
	setFavs []string // recorded "hash:true/false"
}

func (f *fakeData) Collection(ctx context.Context) ([]Album, []Photo, error) {
	return f.albums, f.photos, nil
}
func (f *fakeData) Favourite(ctx context.Context, hash string) bool {
	return f.favs[hash]
}
func (f *fakeData) SetFavourite(ctx context.Context, hash string, value bool) error {
	if f.favs == nil {
		f.favs = map[string]bool{}
	}
	f.favs[hash] = value
	f.setFavs = append(f.setFavs, hash+":"+boolStr(value))
	return nil
}
func (f *fakeData) Info(ctx context.Context) Info { return f.info }

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

type uiEnv struct {
	t     *testing.T
	eng   *Engine
	drv   *display.Headless
	data  *fakeData
	base  time.Time
	nowMu chan struct{} // not used; now is a field
	now   time.Time
}

func newUIEnv(t *testing.T, photos []Photo, albums []Album) *uiEnv {
	t.Helper()
	cvCanvas := canvas.New(1280, 800)
	drv := display.NewHeadless(slog.New(slog.NewTextHandler(io.Discard, nil)), 1280, 800)
	if err := drv.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drv.Close() })

	data := &fakeData{
		albums: albums,
		photos: photos,
		favs:   map[string]bool{},
		info:   Info{DeviceName: "Test Frame", ServerURL: "http://s", ServerReachable: true, Paired: true, Version: "test"},
	}
	for _, p := range photos {
		if p.Favourite {
			data.favs[p.Hash] = true
		}
	}
	cfg := &Config{
		Interval:           45 * time.Second,
		Order:              "sequential",
		Fit:                "contain",
		ShowCaption:        true,
		ShowClock:          true,
		Transition:         "none",
		TransitionDuration: 0,
		Brightness:         0.8,
		SleepOn:            7 * 60,
		SleepOff:           22 * 60,
		Now:                func() time.Time { return time.Now() },
	}
	e := New(cvCanvas, drv, data, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), make(chan touch.Gesture, 8))
	env := &uiEnv{t: t, eng: e, drv: drv, data: data}
	e.Reload(context.Background())
	e.render()
	return env
}

func (e *uiEnv) nowAt(t time.Time) {
	e.eng.cfg.Now = func() time.Time { return t }
}

// gestureT stamps gestures with the injected clock so inactivity
// math is deterministic.
func (e *uiEnv) gestureT() time.Time { return e.eng.cfg.Now() }

func (e *uiEnv) tap(x, y int) {
	e.eng.HandleGesture(touch.Gesture{Kind: touch.KindTap, Pos: [2]int{x, y}, T: e.gestureT()})
}

func (e *uiEnv) swipe(dir touch.Dir) {
	e.eng.HandleGesture(touch.Gesture{Kind: touch.KindSwipe, Dir: dir, T: e.gestureT()})
}

func (e *uiEnv) doubleTap() {
	e.eng.HandleGesture(touch.Gesture{Kind: touch.KindDoubleTap, T: e.gestureT()})
}

func (e *uiEnv) hold(x, y int) {
	e.eng.HandleGesture(touch.Gesture{Kind: touch.KindHold, Pos: [2]int{x, y}, T: e.gestureT()})
}

func (e *uiEnv) onSearchBackgroundTap(x, y int) {
	e.eng.onSearchBackgroundTap(x, y)
}

func (e *uiEnv) px(x, y int) color.NRGBA {
	p, err := e.drv.LastFramePixel(x, y)
	if err != nil {
		e.t.Fatalf("LastFramePixel(%d,%d): %v", x, y, err)
	}
	return p
}

// writePhoto writes a solid-colour PNG to a temp file and returns its
// path (a stand-in for a synced derivative).
func writePhoto(t *testing.T, col color.NRGBA) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.png")
	img := image.NewNRGBA(image.Rect(0, 0, 640, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 640; x++ {
			img.SetNRGBA(x, y, col)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

func testPhotos(t *testing.T, n int) []Photo {
	t.Helper()
	cols := []color.NRGBA{
		{200, 30, 30, 255},
		{30, 200, 30, 255},
		{30, 30, 200, 255},
		{200, 200, 30, 255},
	}
	names := []string{"Alpha shot", "Bravo shot", "Charlie shot"}
	photos := make([]Photo, 0, n)
	for i := 0; i < n; i++ {
		photos = append(photos, Photo{
			Hash:        "h" + string(rune('a'+i)),
			Name:        names[i%len(names)],
			Caption:     "caption " + string(rune('a'+i)),
			AlbumID:     "album1",
			AlbumName:   "Family",
			Uploader:    "Mat",
			CaptureDate: time.Date(2026, 5, 1+i, 10, 0, 0, 0, time.UTC),
			DisplayPath: writePhoto(t, cols[i%len(cols)]),
			ThumbPath:   writePhoto(t, cols[i%len(cols)]),
		})
	}
	// Newest capture first (the canonical collection order).
	sortPhotosByDate(photos)
	return photos
}

// --- tests -----------------------------------------------------------

func TestSlideshowShowsPhotoAndChrome(t *testing.T) {
	photos := testPhotos(t, 2)
	env := newUIEnv(t, photos, []Album{{ID: "album1", Name: "Family", Count: 2}})
	e := env.eng

	if e.Screen() != ScreenSlideshow {
		t.Fatalf("screen = %v", e.Screen())
	}
	if p := e.CurrentPhoto(); p == nil || p.Hash != "hb" || p.Name != "Bravo shot" {
		t.Fatalf("current = %+v (newest first expected: Bravo)", p)
	}
	// Centre pixel is the photo (Bravo = green 30,200,30).
	p := env.px(640, 300)
	if p.G < 150 || p.R > 100 || p.B > 100 {
		t.Errorf("centre pixel = %v, want photo green", p)
	}
	// Caption bar: bottom row is dark chrome.
	if p := env.px(640, 790); p.R > 60 {
		t.Errorf("caption bar pixel = %v, want dark", p)
	}
	// Clock: top-right has a dark backing box (non-photo pixel).
	if p := env.px(1200, 20); p.R > 120 {
		t.Errorf("clock backing pixel = %v, want dark box", p)
	}
}

func TestSlideshowSwipeAdvances(t *testing.T) {
	photos := testPhotos(t, 3)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	// Newest-first list: [hc, hb, ha].
	if e.CurrentPhoto().Hash != "hc" {
		t.Fatalf("start = %s, want hc", e.CurrentPhoto().Hash)
	}
	env.swipe(touch.DirLeft)
	if got := e.CurrentPhoto().Hash; got != "hb" {
		t.Errorf("after swipe left = %s, want hb", got)
	}
	env.swipe(touch.DirRight)
	if got := e.CurrentPhoto().Hash; got != "hc" {
		t.Errorf("after swipe right = %s, want hc", got)
	}
	env.swipe(touch.DirRight)
	if got := e.CurrentPhoto().Hash; got != "ha" {
		t.Errorf("after swipe right = %s, want ha (wrap)", got)
	}
}

func TestControlsRevealAndAutoHide(t *testing.T) {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	env.nowAt(base)

	// Controls start hidden: bottom button row shows the photo.
	if e.controls {
		t.Fatal("controls visible initially")
	}
	env.tap(100, 700)
	if !e.controls {
		t.Fatal("tap should reveal controls")
	}
	// Controls auto-hide 5s after the last activity.
	env.nowAt(base.Add(6 * time.Second))
	e.Tick(base.Add(6 * time.Second))
	if e.controls {
		t.Error("controls should auto-hide after 6s idle")
	}
	// Activity 1s before the tick keeps them visible.
	env.nowAt(base.Add(13 * time.Second))
	env.tap(100, 700)
	env.nowAt(base.Add(14 * time.Second))
	e.Tick(base.Add(14 * time.Second))
	if !e.controls {
		t.Error("controls hidden although activity was 1s ago")
	}
}

func TestDoubleTapOpensDetails(t *testing.T) {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	env.doubleTap()
	if e.Screen() != ScreenDetails {
		t.Fatalf("screen = %v, want details", e.Screen())
	}
	// Panel is present: a pixel in the bottom panel band is grey.
	p := env.px(640, 750)
	if p.R < 10 || p.G < 10 || p.B < 10 {
		t.Errorf("panel pixel = %v, want grey panel", p)
	}
	// Swipe up goes back.
	env.swipe(touch.DirUp)
	if e.Screen() != ScreenSlideshow {
		t.Fatalf("screen = %v, want slideshow", e.Screen())
	}
}

func TestFavouriteToggle(t *testing.T) {
	photos := testPhotos(t, 2)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	env.tap(640, 400) // reveal controls
	// The star button is the 4th of 5 in the bottom row.
	// Find it via the engine's hit buttons.
	var star *hitButton
	for i := range e.buttons {
		if e.buttons[i].kind == btnStar {
			star = &e.buttons[i]
		}
	}
	if star == nil {
		t.Fatal("no star button rendered")
	}
	cx, cy := (star.rect[0]+star.rect[2])/2, (star.rect[1]+star.rect[3])/2
	cur := e.CurrentPhoto().Hash
	env.tap(cx, cy)
	if !env.data.favs[cur] {
		t.Errorf("favourite not set on data (cur=%s)", cur)
	}
	if len(env.data.setFavs) != 1 || env.data.setFavs[0] != cur+":true" {
		t.Errorf("setFavs = %v, want [%s:true]", env.data.setFavs, cur)
	}
	// Unstar.
	env.tap(cx, cy)
	if env.data.favs[cur] {
		t.Error("favourite not unset")
	}
}

func TestGalleryNavigation(t *testing.T) {
	photos := testPhotos(t, 3)
	albums := []Album{
		{ID: "album1", Name: "Family", Count: 3},
		{ID: "album2", Name: "Trips", Count: 0},
	}
	env := newUIEnv(t, photos, albums)
	e := env.eng

	// Slideshow -> gallery via swipe down.
	env.swipe(touch.DirDown)
	if e.Screen() != ScreenGallery {
		t.Fatalf("screen = %v, want gallery", e.Screen())
	}
	// Tap the "Family" album row (3rd row: Slideshow, Favourites, Family).
	var albumBtn *hitButton
	for i := range e.buttons {
		if e.buttons[i].kind == btnAlbum && e.buttons[i].label == "album1" {
			albumBtn = &e.buttons[i]
		}
	}
	if albumBtn == nil {
		t.Fatal("album1 row not found")
	}
	env.tap((albumBtn.rect[0]+albumBtn.rect[2])/2, (albumBtn.rect[1]+albumBtn.rect[3])/2)
	if e.Screen() != ScreenAlbum {
		t.Fatalf("screen = %v, want album", e.Screen())
	}
	// Grid cell tap opens the photo in the slideshow.
	var cell *hitButton
	for i := range e.buttons {
		if e.buttons[i].kind == browsePhoto {
			cell = &e.buttons[i]
			break
		}
	}
	if cell == nil {
		t.Fatal("no grid cell")
	}
	env.tap((cell.rect[0]+cell.rect[2])/2, (cell.rect[1]+cell.rect[3])/2)
	if e.Screen() != ScreenSlideshow || e.listKey != "album1" {
		t.Fatalf("screen=%v listKey=%s, want slideshow on album1", e.Screen(), e.listKey)
	}
	// Swipe down from the slideshow returns to the gallery.
	env.swipe(touch.DirDown)
	if e.Screen() != ScreenGallery {
		t.Fatalf("screen = %v, want gallery", e.Screen())
	}
}

func TestSearchSortAndFilter(t *testing.T) {
	photos := testPhotos(t, 3)
	env := newUIEnv(t, photos, nil)
	e := env.eng

	// Gallery -> Search.
	env.swipe(touch.DirDown)
	var searchBtn *hitButton
	for i := range e.buttons {
		if e.buttons[i].kind == btnSearch {
			searchBtn = &e.buttons[i]
		}
	}
	if searchBtn == nil {
		t.Fatal("search row not found")
	}
	env.tap((searchBtn.rect[0]+searchBtn.rect[2])/2, (searchBtn.rect[1]+searchBtn.rect[3])/2)
	if e.Screen() != ScreenSearch {
		t.Fatalf("screen = %v, want search", e.Screen())
	}

	// Default: by date (newest first = Charlie).
	list := e.listForKey("search")
	if list[0].Hash != "hc" {
		t.Errorf("date sort first = %s, want hc", list[0].Hash)
	}
	// Toggle to name via the title button.
	var sortBtn *hitButton
	for i := range e.buttons {
		if e.buttons[i].kind == btnSort {
			sortBtn = &e.buttons[i]
		}
	}
	if sortBtn == nil {
		t.Fatal("sort button not found")
	}
	env.tap((sortBtn.rect[0]+sortBtn.rect[2])/2, (sortBtn.rect[1]+sortBtn.rect[3])/2)
	if !e.sortByName {
		t.Fatal("sort not toggled")
	}
	list = e.listForKey("search")
	if list[0].Name != "Alpha shot" {
		t.Errorf("name sort first = %s, want Alpha shot", list[0].Name)
	}
	// Letter filter: tap the "B" cell in the right-edge index
	// (letter i occupies y in [i*h/26, (i+1)*h/26)).
	env.onSearchBackgroundTap(e.w-10, e.h/26+3)
	list = e.listForKey("search")
	if len(list) != 1 || list[0].Hash != "hb" {
		t.Errorf("filtered list = %+v, want only hb", list)
	}
}

func TestSettingsHoldToApply(t *testing.T) {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	origInterval := e.cfg.Interval

	// Gallery -> Settings.
	env.swipe(touch.DirDown)
	var settingsBtn *hitButton
	for i := range e.buttons {
		if e.buttons[i].kind == btnSettings {
			settingsBtn = &e.buttons[i]
		}
	}
	if settingsBtn == nil {
		t.Fatal("settings row not found")
	}
	env.tap((settingsBtn.rect[0]+settingsBtn.rect[2])/2, (settingsBtn.rect[1]+settingsBtn.rect[3])/2)
	if e.Screen() != ScreenSettings {
		t.Fatalf("screen = %v, want settings", e.Screen())
	}

	applied := false
	e.cfg.ApplySettings = func(s Settings) error {
		applied = true
		return nil
	}

	// Stage: tap slideshow-interval row (cycles 45s -> 1m).
	var intervalBtn *hitButton
	for i := range e.buttons {
		if e.buttons[i].kind == btnSetting+1 {
			intervalBtn = &e.buttons[i]
		}
	}
	if intervalBtn == nil {
		t.Fatal("interval row not found")
	}
	env.tap((intervalBtn.rect[0]+intervalBtn.rect[2])/2, (intervalBtn.rect[1]+intervalBtn.rect[3])/2)
	if e.staged.SlideshowInterval != time.Minute {
		t.Errorf("staged interval = %v, want 1m", e.staged.SlideshowInterval)
	}
	if applied {
		t.Fatal("staging must not apply")
	}

	// A plain TAP on apply does nothing.
	var applyBtn *hitButton
	for i := range e.buttons {
		if e.buttons[i].kind == btnApply {
			applyBtn = &e.buttons[i]
		}
	}
	if applyBtn == nil {
		t.Fatal("apply button not found")
	}
	env.tap((applyBtn.rect[0]+applyBtn.rect[2])/2, (applyBtn.rect[1]+applyBtn.rect[3])/2)
	if applied {
		t.Fatal("tap must not apply (hold protection)")
	}
	// A HOLD applies.
	env.hold((applyBtn.rect[0]+applyBtn.rect[2])/2, (applyBtn.rect[1]+applyBtn.rect[3])/2)
	if !applied {
		t.Fatal("hold should apply")
	}
	if e.cfg.Interval != time.Minute {
		t.Errorf("live interval = %v, want 1m", e.cfg.Interval)
	}
	_ = origInterval
}

func TestInactivityReturnsToSlideshow(t *testing.T) {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	env.nowAt(base)
	env.swipe(touch.DirDown) // gallery
	if e.Screen() != ScreenGallery {
		t.Fatal("not in gallery")
	}
	// 121s idle -> back to the slideshow.
	later := base.Add(121 * time.Second)
	env.nowAt(later)
	e.Tick(later)
	if e.Screen() != ScreenSlideshow {
		t.Errorf("screen = %v, want slideshow after 120s idle", e.Screen())
	}
}

func TestSleepScreen(t *testing.T) {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	e.SetAsleep(true)
	if e.Screen() != ScreenSleep {
		t.Fatalf("screen = %v, want sleep", e.Screen())
	}
	// Black frame.
	if p := env.px(640, 400); p.R != 0 || p.G != 0 || p.B != 0 {
		t.Errorf("sleep pixel = %v, want black", p)
	}
	e.SetAsleep(false)
	if e.Screen() != ScreenSlideshow {
		t.Fatalf("screen = %v, want slideshow after wake", e.Screen())
	}
}

func TestEmptyStates(t *testing.T) {
	// Unpaired.
	env := newUIEnv(t, nil, nil)
	env.data.info.Paired = false
	e := env.eng
	e.Reload(context.Background())
	e.render()
	// No panic + screen stays slideshow.
	if e.Screen() != ScreenSlideshow {
		t.Fatalf("screen = %v", e.Screen())
	}
	// Paired but no photos.
	env2 := newUIEnv(t, nil, nil)
	env2.eng.Reload(context.Background())
	env2.eng.render()
}

func TestThrottleDoublesInterval(t *testing.T) {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	if got := e.effectiveInterval(); got != 45*time.Second {
		t.Errorf("interval = %v, want 45s", got)
	}
	e.SetThrottled(true)
	if got := e.effectiveInterval(); got != 90*time.Second {
		t.Errorf("throttled interval = %v, want 90s", got)
	}
	e.SetThrottled(false)
	if got := e.effectiveInterval(); got != 45*time.Second {
		t.Errorf("interval after unthrottle = %v, want 45s", got)
	}
}

func TestReloadReflectsActivation(t *testing.T) {
	photos := testPhotos(t, 2)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	if len(e.list) != 2 {
		t.Fatalf("list = %d, want 2", len(e.list))
	}
	// Simulate a new activation that adds a photo.
	photos = append(photos, testPhotos(t, 1)[0])
	env.data.photos = photos
	e.Reload(context.Background())
	if len(e.list) != 3 {
		t.Errorf("list after reload = %d, want 3", len(e.list))
	}
}

func TestFavouritesList(t *testing.T) {
	photos := testPhotos(t, 3)
	photos[0].Favourite = true
	photos[2].Favourite = true
	env := newUIEnv(t, photos, nil)
	e := env.eng
	e.goSlideshow("favourites", 0)
	if len(e.list) != 2 {
		t.Fatalf("favourites list = %d, want 2", len(e.list))
	}
}
