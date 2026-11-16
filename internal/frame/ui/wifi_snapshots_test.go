package ui

// TestRenderWifiScreens renders the real Wi-Fi setup screens to PNG
// (verification aid, not a regression test — safe to delete). Run:
//
//	go test ./internal/frame/ui/ -run TestRenderWifiScreens
//
// Output: /tmp/wifi-snap/*.png at 1280x800 (the Waveshare panel's
// native resolution). The backend is a stub (no radio on the dev
// box); every pixel comes from the real render code.

import (
	"context"
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
	"github.com/matthewgall/openframe/internal/wifi"
)

func TestRenderWifiScreens(t *testing.T) {
	outDir := "/tmp/wifi-snap"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	cv := canvas.New(1280, 800)
	drv := display.NewHeadless(discard, 1280, 800)
	if err := drv.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drv.Close() })

	fw := &fakeWifi{
		info: wifi.Info{Available: true, Backend: "wpa_supplicant", Interface: "wlan0"},
		nets: []wifi.Network{
			{SSID: "GallFamily", Signal: 84, Encrypted: true},
			{SSID: "GallFamily-5G", Signal: 61, Encrypted: true},
			{SSID: "NextDoor", Signal: 47, Encrypted: true},
			{SSID: "CoffeeShop", Signal: 38, Encrypted: false},
			{SSID: "FreeWiFi", Signal: 22, Encrypted: false},
		},
	}
	// No photos: a fresh frame has nothing to show, so the UI is in
	// its empty/pairing state — the pairing screen renders there.
	data := &fakeData{
		photos: nil,
		favs:   map[string]bool{},
		info:   Info{DeviceName: "Living room", ServerURL: "https://openframe.gall.family", Paired: false, ServerReachable: false, Version: "0.1.0"},
	}
	cfg := &Config{
		Interval:           45 * time.Second,
		Order:              "sequential",
		Fit:                "contain",
		ShowCaption:        true,
		ShowClock:          true,
		Transition:         "none",
		Brightness:         0.8,
		SleepOn:            7 * 60,
		SleepOff:           22 * 60,
		Now:                time.Now,
	}
	e := New(cv, drv, data, cfg, discard, make(chan touch.Gesture, 8))
	e.SetWifi(fw)
	e.Reload(context.Background())

	tap := func(x, y int) { e.HandleGesture(touch.Gesture{Kind: touch.KindTap, Pos: [2]int{x, y}, T: time.Now()}) }
	tapBtn := func(kind int, label string) {
		for i := range e.buttons {
			b := &e.buttons[i]
			if b.kind == kind && (label == "" || b.label == label) {
				tap((b.rect[0]+b.rect[2])/2, (b.rect[1]+b.rect[3])/2)
				return
			}
		}
		t.Fatalf("button not found: kind=%d label=%q", kind, label)
	}
	save := func(name string) {
		f, err := os.Create(filepath.Join(outDir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, cv.Image()); err != nil {
			t.Fatal(err)
		}
		t.Log("wrote", outDir, name)
	}
	pump := func() {
		for i := 0; i < 40; i++ {
			select {
			case r := <-e.async:
				e.mu.Lock()
				e.handleAsync(r)
				e.mu.Unlock()
			default:
				if i > 8 {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}

	// Screen 1: the true first-boot state — unreachable, NO pairing
	// code (codes are created on the server), the Wi-Fi hint carries
	// the way forward.
	e.mu.Lock()
	e.render()
	e.mu.Unlock()
	save("1-firstboot-no-network.png")

	// Screen 2: the network list (tap the hint; scan completes).
	tapBtn(btnWifiSetup, "")
	pump()
	if e.Screen() != ScreenWifiList {
		t.Fatalf("screen = %v, want wifi list", e.Screen())
	}
	save("2-wifi-list.png")

	// Screen 3: passphrase keyboard, masked, with a partial entry.
	tapBtn(btnWifiNet, "GallFamily")
	if e.Screen() != ScreenWifiText {
		t.Fatalf("screen = %v, want keyboard", e.Screen())
	}
	for _, ch := range []string{"g", "a", "l", "l", "o", "w"} {
		tapBtn(btnKey, ch)
	}
	save("3-wifi-keyboard-masked.png")

	// Screen 4: same field with Show toggled.
	tapBtn(btnKeyShow, "")
	save("4-wifi-keyboard-shown.png")

	// Screen 5: the network is back — the pairing code now exists
	// (created on the server) and the hint is gone.
	e.mu.Lock()
	data.info.ServerReachable = true
	e.mu.Unlock()
	e.SetPairing("KF3T9Q2X")
	save("5-pairing-code-online.png")
}
