package display

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
		t.Errorf("auto resolved to %q, want headless in this release", d.Name())
	}
}

func TestFactoryRejectsUnknown(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"drm", "vulkan"} {
		if _, err := New(ctx, Options{Name: name, Log: testLog()}); err == nil {
			t.Errorf("driver %q should be rejected in this release", name)
		}
	}
}

func TestHeadlessLifecycle(t *testing.T) {
	ctx := context.Background()
	h := NewHeadless(testLog(), 1280, 800)

	// Present before Init must fail.
	if err := h.Present(ctx, ImageRef{Path: "x.jpg"}); err == nil {
		t.Error("Present before Init should fail")
	}

	if err := h.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.Present(ctx, ImageRef{Path: "/cache/abc.jpg", Hash: "abc"}); err != nil {
		t.Fatal(err)
	}
	last := h.LastPresented()
	if last == nil || last.Path != "/cache/abc.jpg" || last.Hash != "abc" {
		t.Errorf("last = %+v", last)
	}
	if h.Presents() != 1 {
		t.Errorf("presents = %d", h.Presents())
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

func TestHeadlessBrightnessAndPower(t *testing.T) {
	ctx := context.Background()
	h := NewHeadless(testLog(), 640, 480)
	h.Init(ctx)
	defer h.Close()

	if err := h.SetBrightness(ctx, 0.3); err != nil {
		t.Fatal(err)
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
	// Agent keeps working while the display is off: present still works.
	if err := h.Present(ctx, ImageRef{Path: "y.jpg"}); err != nil {
		t.Errorf("Present while display off: %v", err)
	}
}

func TestHeadlessRejectsEmptyPath(t *testing.T) {
	ctx := context.Background()
	h := NewHeadless(testLog(), 640, 480)
	h.Init(ctx)
	defer h.Close()
	if err := h.Present(ctx, ImageRef{}); err == nil {
		t.Error("empty image path should be rejected")
	}
}
