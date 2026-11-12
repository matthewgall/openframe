package display

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
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func solidFrame(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i+3 < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

func TestFactoryHeadless(t *testing.T) {
	ctx := context.Background()
	d, err := New(ctx, Options{Name: "headless", Width: 1280, Height: 800, Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Name() != "headless" {
		t.Errorf("name = %q", d.Name())
	}
	info := d.Info()
	if info.ResolutionWidth != 1280 || info.ResolutionHeight != 800 {
		t.Errorf("info = %+v", info)
	}
}

func TestFactoryAutoFallsBackToHeadless(t *testing.T) {
	ctx := context.Background()
	d, err := New(ctx, Options{Name: "auto", Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Name() != "headless" {
		t.Errorf("auto resolved to %q, want headless when no hardware", d.Name())
	}
}

func TestHeadlessLifecycle(t *testing.T) {
	ctx := context.Background()
	h := NewHeadless(testLog(), 1280, 800)

	// PresentFrame before Init must fail.
	if err := h.PresentFrame(ctx, solidFrame(1280, 800, color.NRGBA{1, 2, 3, 255})); err == nil {
		t.Error("PresentFrame before Init should fail")
	}

	if err := h.Init(ctx); err != nil {
		t.Fatal(err)
	}
	// Wrong-size frame is rejected.
	if err := h.PresentFrame(ctx, solidFrame(10, 10, color.NRGBA{0, 0, 0, 255})); err == nil {
		t.Error("wrong-size frame should fail")
	}

	f := solidFrame(1280, 800, color.NRGBA{9, 8, 7, 255})
	if err := h.PresentFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	if h.Presents() != 1 {
		t.Errorf("presents = %d", h.Presents())
	}
	// The driver must have copied the pixels: mutating the source
	// after present does not change what the driver holds.
	f.SetNRGBA(10, 10, color.NRGBA{200, 200, 200, 255})
	p, err := h.LastFramePixel(10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if p != (color.NRGBA{9, 8, 7, 255}) {
		t.Errorf("pixel after source mutation = %v, want copied value", p)
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	// Double close is a no-op.
	if err := h.Close(); err != nil {
		t.Errorf("double close: %v", err)
	}
	// Init after close is an error.
	if err := h.Init(ctx); err == nil {
		t.Error("Init after Close should fail")
	}
}

func TestHeadlessBrightnessPowerRotation(t *testing.T) {
	ctx := context.Background()
	h := NewHeadless(testLog(), 640, 480)
	h.Init(ctx)
	defer h.Close()

	if err := h.SetBrightness(ctx, 0.3); err != nil {
		t.Fatal(err)
	}
	if h.Brightness() != 0.3 {
		t.Errorf("brightness = %v", h.Brightness())
	}
	for _, v := range []float64{-0.1, 1.5} {
		if err := h.SetBrightness(ctx, v); err == nil {
			t.Errorf("brightness %v should be rejected", v)
		}
	}

	if err := h.SetPower(ctx, false); err != nil {
		t.Fatal(err)
	}
	if h.Power() {
		t.Error("power should be off")
	}
	if err := h.SetPower(ctx, true); err != nil {
		t.Fatal(err)
	}

	for _, deg := range []int{90, 180, 270, 0} {
		if err := h.SetRotation(ctx, deg); err != nil {
			t.Fatalf("rotation %d: %v", deg, err)
		}
		if h.Rotation() != deg {
			t.Errorf("rotation = %d, want %d", h.Rotation(), deg)
		}
	}
	if err := h.SetRotation(ctx, 45); err == nil {
		t.Error("rotation 45 should be rejected")
	}
}

func TestHeadlessSnapshots(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	h := NewHeadlessWithSnapshots(testLog(), 64, 32, SnapshotConfig{Dir: dir, Interval: 0})
	if err := h.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if err := h.PresentFrame(ctx, solidFrame(64, 32, color.NRGBA{12, 34, 56, 255})); err != nil {
		t.Fatal(err)
	}
	// latest.png must exist and decode to the presented colour.
	f, err := os.Open(filepath.Join(dir, "latest.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 64 || img.Bounds().Dy() != 32 {
		t.Errorf("snapshot size = %v", img.Bounds())
	}
	r, g, b, _ := img.At(5, 5).RGBA() // png decodes to RGBA, not NRGBA
	if uint8(r>>8) != 12 || uint8(g>>8) != 34 || uint8(b>>8) != 56 {
		t.Errorf("snapshot pixel = %v", img.At(5, 5))
	}
	// A ring entry was written too.
	entries, _ := os.ReadDir(dir)
	pngs := 0
	for _, e := range entries {
		if len(e.Name()) > 4 && e.Name()[len(e.Name())-4:] == ".png" {
			pngs++
		}
	}
	if pngs < 2 {
		t.Errorf("expected latest + ring entries, found %d pngs: %v", pngs, entries)
	}
}

func TestHeadlessSnapshotDirFailureDegrades(t *testing.T) {
	// A snapshot dir that cannot be created must not break the driver.
	h := NewHeadlessWithSnapshots(testLog(), 64, 32,
		SnapshotConfig{Dir: "/dev/null/impossible"})
	if err := h.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.PresentFrame(context.Background(), solidFrame(64, 32, color.NRGBA{1, 1, 1, 255})); err != nil {
		t.Errorf("present with bad snapshot dir should still work: %v", err)
	}
}
