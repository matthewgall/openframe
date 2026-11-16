package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/frame/touch"
	"github.com/matthewgall/openframe/internal/wifi"
)

// fakeWifi is an in-memory wifi.Backend for UI tests.
type fakeWifi struct {
	info         wifi.Info
	nets         []wifi.Network
	connectErr   error
	connected    string
	connectCalls []struct{ ssid, psk string }
}

func (f *fakeWifi) Info(context.Context) (wifi.Info, error) {
	i := f.info
	if f.connected != "" {
		i.ConnectedSSID = f.connected
	}
	return i, nil
}
func (f *fakeWifi) Scan(context.Context) ([]wifi.Network, error) { return f.nets, nil }
func (f *fakeWifi) Connect(_ context.Context, ssid, psk string) error {
	f.connectCalls = append(f.connectCalls, struct{ ssid, psk string }{ssid, psk})
	if f.connectErr != nil {
		return f.connectErr
	}
	f.connected = ssid
	return nil
}
func (f *fakeWifi) Forget(context.Context, string) error { return nil }
func (f *fakeWifi) AutoConnect(context.Context) error    { return nil }

// pumpAsync delivers one pending async result to the engine (the test
// harness runs gestures synchronously, without the Run loop).
// pumpAsync drains the async channel until quiet. The fake backend
// completes instantly; a 100ms quiet window is far longer than any
// goroutine scheduling latency here.
func (e *uiEnv) pumpAsync() {
	quiet := 0
	for quiet < 20 { // 20 x 5ms
		select {
		case r := <-e.eng.async:
			e.eng.mu.Lock()
			e.eng.handleAsync(r)
			e.eng.mu.Unlock()
			quiet = 0
		case <-time.After(5 * time.Millisecond):
			quiet++
		}
	}
}

func (e *uiEnv) button(kind int) *hitButton {
	for i := range e.eng.buttons {
		if e.eng.buttons[i].kind == kind {
			return &e.eng.buttons[i]
		}
	}
	return nil
}

func (e *uiEnv) buttonLabel(kind int, label string) *hitButton {
	for i := range e.eng.buttons {
		b := &e.eng.buttons[i]
		if b.kind == kind && b.label == label {
			return b
		}
	}
	e.t.Logf("wanted kind=%d label=%q; labels=%v", kind, label, labelsOf(e.eng.buttons))
	return nil
}

func labelsOf(bs []hitButton) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.label)
	}
	return out
}

func (e *uiEnv) tapButton(b *hitButton) {
	if b == nil {
		var kinds []int
		for _, x := range e.eng.buttons {
			kinds = append(kinds, x.kind)
		}
		e.t.Fatalf("button not found on screen; screen=%v busy=%v kinds=%v",
			e.eng.Screen(), e.eng.wifiBusy, kinds)
	}
	e.tap((b.rect[0]+b.rect[2])/2, (b.rect[1]+b.rect[3])/2)
}

func wifiTestEnv(t *testing.T, fw *fakeWifi) *uiEnv {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil)
	env.eng.SetWifi(fw)
	return env
}

// TestWifiSettingsRowAndList: settings row shows live state, tapping
// it opens the list, the scan result renders, and an open network
// connects directly (empty psk).
func TestWifiSettingsRowAndList(t *testing.T) {
	fw := &fakeWifi{
		info: wifi.Info{Available: true, Backend: "fake", Interface: "wlan0"},
		nets: []wifi.Network{
			{SSID: "HomeNet", Signal: 82, Encrypted: true},
			{SSID: "CoffeeShop", Signal: 55, Encrypted: false},
		},
	}
	env := wifiTestEnv(t, fw)
	e := env.eng

	// To the gallery, then to settings.
	env.swipe(touch.DirDown)
	env.tapButton(env.button(btnSettings))
	if e.Screen() != ScreenSettings {
		t.Fatalf("screen = %v", e.Screen())
	}
	// The Wi-Fi row (index 8) is below the fold on 1280x800: scroll.
	env.swipe(touch.DirDown)
	wifiRow := env.button(btnSetting + 8)
	if wifiRow == nil {
		t.Fatal("Wi-Fi settings row not visible")
	}
	if wifiRow.label != "Wi-Fi" {
		t.Fatalf("row label = %q", wifiRow.label)
	}
	env.tapButton(wifiRow)
	if e.Screen() != ScreenWifiList {
		t.Fatalf("screen = %v, want wifi list", e.Screen())
	}
	env.pumpAsync() // scan completes

	// Both networks rendered as buttons.
	if env.buttonLabel(btnWifiNet, "HomeNet") == nil || env.buttonLabel(btnWifiNet, "CoffeeShop") == nil {
		t.Fatalf("networks not rendered: %+v", e.buttons)
	}
	if env.button(btnWifiHidden) == nil {
		t.Fatal("hidden-network row missing")
	}

	// Tapping the OPEN network connects directly with an empty psk.
	env.tapButton(env.buttonLabel(btnWifiNet, "CoffeeShop"))
	env.pumpAsync()
	if len(fw.connectCalls) != 1 || fw.connectCalls[0].ssid != "CoffeeShop" || fw.connectCalls[0].psk != "" {
		t.Fatalf("connect calls = %+v", fw.connectCalls)
	}
	// Success returns to settings, and the row now shows the SSID.
	if e.Screen() != ScreenSettings {
		t.Fatalf("after connect screen = %v, want settings", e.Screen())
	}
	env.swipe(touch.DirDown) // scroll the row back into view
	wifiRow = env.button(btnSetting + 8)
	if wifiRow == nil || wifiRow.label != "Wi-Fi" {
		t.Fatal("Wi-Fi row missing after connect")
	}
	if e.wifiStateValue() != "CoffeeShop" {
		t.Fatalf("settings row value = %q, want CoffeeShop", e.wifiStateValue())
	}
}

// TestWifiPasswordEntry: an encrypted network goes through the
// keyboard; the typed passphrase is delivered on connect.
func TestWifiPasswordEntry(t *testing.T) {
	fw := &fakeWifi{
		info: wifi.Info{Available: true, Backend: "fake", Interface: "wlan0"},
		nets: []wifi.Network{{SSID: "HomeNet", Signal: 82, Encrypted: true}},
	}
	env := wifiTestEnv(t, fw)
	e := env.eng

	env.swipe(touch.DirDown)
	env.tapButton(env.button(btnSettings))
	env.swipe(touch.DirDown)
	env.tapButton(env.button(btnSetting + 8))
	env.pumpAsync()

	env.tapButton(env.buttonLabel(btnWifiNet, "HomeNet"))
	if e.Screen() != ScreenWifiText || !e.wifiPsk {
		t.Fatalf("screen = %v pskMode = %v", e.Screen(), e.wifiPsk)
	}

	// Type "s3c": find the letter buttons and tap them.
	typeLetter := func(ch byte) {
		env.tapButton(env.buttonLabel(btnKey, string(ch)))
	}
	typeLetter('s')
	typeLetter('e')
	typeLetter('c')
	if e.wifiText != "sec" {
		t.Fatalf("typed = %q, want sec", e.wifiText)
	}
	// Backspace removes one.
	env.tapButton(env.button(btnKeyBack))
	if e.wifiText != "se" {
		t.Fatalf("after backspace = %q", e.wifiText)
	}
	typeLetter('c')

	env.tapButton(env.button(btnKeyDone))
	env.pumpAsync()
	if len(fw.connectCalls) != 1 || fw.connectCalls[0].ssid != "HomeNet" || fw.connectCalls[0].psk != "sec" {
		t.Fatalf("connect calls = %+v", fw.connectCalls)
	}
	if e.Screen() != ScreenSettings {
		t.Fatalf("screen = %v, want settings", e.Screen())
	}
}

// TestWifiConnectFailure: a failed connect lands back on the list with
// the error shown, not a silent return.
func TestWifiConnectFailure(t *testing.T) {
	fw := &fakeWifi{
		info:       wifi.Info{Available: true, Backend: "fake", Interface: "wlan0"},
		nets:       []wifi.Network{{SSID: "HomeNet", Signal: 82, Encrypted: false}},
		connectErr: errors.New("association rejected"),
	}
	env := wifiTestEnv(t, fw)
	e := env.eng

	env.swipe(touch.DirDown)
	env.tapButton(env.button(btnSettings))
	env.swipe(touch.DirDown)
	env.tapButton(env.button(btnSetting + 8))
	env.pumpAsync()

	homeNet := env.buttonLabel(btnWifiNet, "HomeNet")
	if homeNet == nil {
		t.Fatalf("HomeNet button not rendered; screen=%v busy=%v msg=%q buttons=%+v",
			e.Screen(), e.wifiBusy, e.wifiMsg, e.buttons)
	}
	env.tapButton(homeNet)
	env.pumpAsync()
	if e.Screen() != ScreenWifiList {
		t.Fatalf("screen = %v, want list after failure", e.Screen())
	}
	if len(fw.connectCalls) == 0 {
		t.Fatalf("connect was never called; screen=%v msg=%q", e.Screen(), e.wifiMsg)
	}
	if e.wifiMsg == "" || !strings.Contains(e.wifiMsg, "association rejected") {
		t.Fatalf("error not shown: %q", e.wifiMsg)
	}
}

// TestWifiPairingHint: unpaired + server unreachable + wifi available
// shows the setup hint on BOTH pairing states; reachable hides it.
func TestWifiPairingHint(t *testing.T) {
	for _, reachable := range []bool{true, false} {
		fw := &fakeWifi{info: wifi.Info{Available: true, Backend: "fake", Interface: "wlan0"}}
		env := newUIEnv(t, nil, nil) // no photos: empty state
		env.eng.SetWifi(fw)
		env.data.info.Paired = false
		env.data.info.ServerReachable = reachable
		env.eng.Reload(context.Background())
		env.eng.mu.Lock()
		env.eng.render()
		env.eng.mu.Unlock()

		btn := env.button(btnWifiSetup)
		if reachable && btn != nil {
			t.Fatalf("reachable: hint must not render")
		}
		if !reachable && btn == nil {
			t.Fatalf("unreachable: hint missing")
		}
		if !reachable {
			env.tapButton(btn)
			if env.eng.Screen() != ScreenWifiList {
				t.Fatalf("tap hint: screen = %v", env.eng.Screen())
			}
		}
	}
}

// TestWifiHiddenNetwork: hidden entry collects an SSID, then its
// passphrase, then connects with both.
func TestWifiHiddenNetwork(t *testing.T) {
	fw := &fakeWifi{
		info: wifi.Info{Available: true, Backend: "fake", Interface: "wlan0"},
		nets: nil,
	}
	env := wifiTestEnv(t, fw)
	e := env.eng

	env.swipe(touch.DirDown)
	env.tapButton(env.button(btnSettings))
	env.swipe(touch.DirDown)
	env.tapButton(env.button(btnSetting + 8))
	env.pumpAsync()

	env.tapButton(env.button(btnWifiHidden))
	if e.Screen() != ScreenWifiText || e.wifiPsk {
		t.Fatalf("screen = %v pskMode = %v, want SSID entry", e.Screen(), e.wifiPsk)
	}
	typeName := func(name string) {
		for _, ch := range name {
			env.tapButton(env.buttonLabel(btnKey, string(ch)))
		}
	}
	typeCapital := func(name string) {
		// Capitalise the first letter via the shift toggle (the
		// keyboard buttons carry lowercase base letters).
		env.tapButton(env.button(btnKeyShift))
		typeName(strings.ToLower(name[:1]))
		env.tapButton(env.button(btnKeyShift))
		if len(name) > 1 {
			typeName(name[1:])
		}
	}
	typeCapital("Sec")
	if e.wifiText != "Sec" {
		t.Fatalf("typed = %q, want Sec (shift handling)", e.wifiText)
	}
	// Done on an empty SSID must refuse.
	e.wifiText = ""
	env.tapButton(env.button(btnKeyDone))
	if e.Screen() != ScreenWifiText {
		t.Fatalf("empty SSID must stay on the entry screen")
	}
	typeCapital("Sec")
	env.tapButton(env.button(btnKeyDone))
	if e.Screen() != ScreenWifiText || !e.wifiPsk || e.wifiSSID != "Sec" {
		t.Fatalf("after SSID done: screen=%v pskMode=%v ssid=%q", e.Screen(), e.wifiPsk, e.wifiSSID)
	}
	// Passphrase, then connect.
	typeName("pw")
	env.tapButton(env.button(btnKeyDone))
	env.pumpAsync()
	if len(fw.connectCalls) != 1 || fw.connectCalls[0].ssid != "Sec" || fw.connectCalls[0].psk != "pw" {
		t.Fatalf("connect calls = %+v", fw.connectCalls)
	}
}

// TestWifiUnavailable: a nil backend hides everything (no row value
// change, no hint).
func TestWifiUnavailable(t *testing.T) {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil) // wifi never set
	e := env.eng
	if e.wifi != nil {
		t.Fatal("test harness must start without a wifi backend")
	}
	env.swipe(touch.DirDown)
	env.tapButton(env.button(btnSettings))
	env.swipe(touch.DirDown)
	row := env.button(btnSetting + 8)
	if row == nil {
		t.Fatal("Wi-Fi row should still exist")
	}
	if e.wifiStateValue() != "unavailable" {
		t.Fatalf("value = %q, want unavailable", e.wifiStateValue())
	}
	env.tapButton(row)
	if e.Screen() != ScreenSettings {
		t.Fatalf("tapping an unavailable Wi-Fi row must not navigate, screen = %v", e.Screen())
	}
}
