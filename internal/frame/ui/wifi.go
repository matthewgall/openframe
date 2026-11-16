package ui

// Wi-Fi setup (ADR-0014): the screens and actions behind the settings
// row and the pairing-screen hint. Scanning and connecting run in
// goroutines (they take seconds); results come back on e.async so the
// engine stays a single-goroutine state machine.
//
// The backend is nil unless the platform supports it, in which case
// every Wi-Fi affordance is hidden.

import (
	"context"
	"time"

	"github.com/matthewgall/openframe/internal/wifi"
)

// async kinds
const (
	asyncScan = iota
	asyncConnect
)

type asyncResult struct {
	kind int
	nets []wifi.Network
	err  error
	ssid string    // connect target
	info wifi.Info // refreshed state on connect success
}

// SetWifi installs the Wi-Fi backend (nil hides the feature). Call
// before Run.
func (e *Engine) SetWifi(b wifi.Backend) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.async == nil {
		e.async = make(chan asyncResult, 8)
	}
	e.wifi = b
	if b != nil {
		info, err := b.Info(context.Background())
		if err == nil {
			e.wifiInfo = info
		}
		e.log.Info("wifi backend ready", "backend", info.Backend, "interface", info.Interface)
	}
}

// wifiStateValue is the settings-row value (live state).
func (e *Engine) wifiStateValue() string {
	if e.wifi == nil {
		return "unavailable"
	}
	info := e.wifiInfo
	if info.WiredUp {
		return "wired"
	}
	if info.ConnectedSSID != "" {
		return info.ConnectedSSID
	}
	if !info.Available {
		return "no radio"
	}
	return "not connected"
}

// goWifiList opens the network list from (0) settings or (1) the
// pairing screen, and starts a fresh scan.
func (e *Engine) goWifiList(from int) {
	e.wifiFrom = from
	e.wifiNets = nil
	e.wifiTop = 0
	e.wifiMsg = ""
	e.screen = ScreenWifiList
	e.startWifiScan()
	e.render()
}

// goWifiText opens the text screen for a hidden SSID (pskMode false)
// or the passphrase of ssid (pskMode true).
func (e *Engine) goWifiText(ssid string, pskMode bool) {
	e.wifiSSID = ssid
	e.wifiPsk = pskMode
	e.wifiText = ""
	e.wifiShow = false
	e.wifiShift = false
	e.wifiMsg = ""
	e.screen = ScreenWifiText
	e.render()
}

// goWifiBack returns to wherever the flow started from.
func (e *Engine) goWifiBack() {
	e.wifiBusy = false
	e.wifiMsg = ""
	if e.wifiFrom == 0 {
		e.screen = ScreenSettings
	} else {
		// Back to the empty/pairing state (the slideshow renders it).
		e.goSlideshow("all", -1)
	}
	e.render()
}

func (e *Engine) startWifiScan() {
	if e.wifi == nil || e.wifiBusy {
		return
	}
	e.wifiBusy = true
	// Never clobber a more important status (e.g. a connect failure).
	if e.wifiMsg == "" {
		e.wifiMsg = "Scanning…"
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		nets, err := e.wifi.Scan(ctx)
		e.async <- asyncResult{kind: asyncScan, nets: nets, err: err}
	}()
}

func (e *Engine) startWifiConnect(ssid, psk string) {
	if e.wifi == nil || e.wifiBusy {
		return
	}
	e.wifiBusy = true
	e.wifiMsg = "Connecting to " + ssid + "…"
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		err := e.wifi.Connect(ctx, ssid, psk)
		var info wifi.Info
		if err == nil {
			// Refresh state for the settings row.
			if i, ierr := e.wifi.Info(ctx); ierr == nil {
				info = i
			}
		}
		e.async <- asyncResult{kind: asyncConnect, err: err, ssid: ssid, info: info}
	}()
}

// handleAsync processes completed Wi-Fi work. The engine mutex is
// taken by the Run loop around this; the screen may have changed
// (inactivity timeout) while the work ran, in which case we only
// update state and do not navigate.
func (e *Engine) handleAsync(r asyncResult) {
	switch r.kind {
	case asyncScan:
		e.wifiBusy = false
		if e.screen != ScreenWifiList {
			return
		}
		if r.err != nil {
			e.wifiMsg = "Scan failed: " + shortErr(r.err)
			e.render()
			return
		}
		e.wifiNets = r.nets
		// Keep a user-facing error from a failed connect; only
		// replace the scan's own status line.
		if e.wifiMsg == "" || e.wifiMsg == "Scanning…" {
			if len(r.nets) == 0 {
				e.wifiMsg = "No networks found — scan again or enter a hidden network."
			} else {
				e.wifiMsg = ""
			}
		}
		e.render()
	case asyncConnect:
		e.wifiBusy = false
		if r.info.Backend != "" || r.err == nil {
			e.wifiInfo = r.info
		}
		switch e.screen {
		case ScreenWifiText, ScreenWifiList:
			if r.err != nil {
				// Show the failure on the list; the user can retry.
				e.wifiMsg = "Connect failed: " + shortErr(r.err)
				e.screen = ScreenWifiList
				e.startWifiScan()
				e.render()
				return
			}
			if e.wifiFrom == 0 {
				e.screen = ScreenSettings
			} else {
				e.goSlideshow("all", -1) // pairing screen: connection restored
			}
			e.render()
		default:
			// Timed out to the slideshow meanwhile: state updated,
			// no navigation.
		}
	}
}

// Keyboard actions (buttons on the text screen). Passphrases top out
// at 63 characters (the WPA2-PSK limit); hidden names at 32 (the
// IEEE 802.11 SSID limit) — but we simply cap at 63 for both.
func (e *Engine) keyPress(r rune) {
	if e.wifiText == "" && r == ' ' {
		return // no leading space
	}
	if len(runesOf(e.wifiText)) < 63 {
		e.wifiText += string(r)
	}
	e.render()
}

func (e *Engine) keyBackspace() {
	r := runesOf(e.wifiText)
	if len(r) > 0 {
		e.wifiText = string(r[:len(r)-1])
	}
	e.render()
}

func (e *Engine) toggleShow() {
	e.wifiShow = !e.wifiShow
	e.render()
}

func (e *Engine) toggleShift() {
	e.wifiShift = !e.wifiShift
	e.render()
}

func (e *Engine) keyDone() {
	switch {
	case !e.wifiPsk:
		// Hidden SSID entered: go to its passphrase screen.
		ssid := e.wifiText
		if ssid == "" {
			e.wifiMsg = "Enter a network name first."
			e.render()
			return
		}
		e.goWifiText(ssid, true)
	case e.wifiPsk:
		// Connect. An empty passphrase is allowed (open network the
		// user picked from the list cannot reach this path, but a
		// hidden entry can).
		e.startWifiConnect(e.wifiSSID, e.wifiText)
	}
}

func runesOf(s string) []rune {
	return []rune(s)
}

// networkEncrypted looks a tapped SSID up in the last scan. Unknown
// networks assume encrypted (asking for a passphrase is the safe
// failure: an open network with no password will just reject one).
func (e *Engine) networkEncrypted(ssid string) bool {
	for _, n := range e.wifiNets {
		if n.SSID == ssid {
			return n.Encrypted
		}
	}
	return true
}

// WifiAutoConnect resumes saved networks on boot (the frame wiring
// calls it in the background): a powered-off frame rejoins its
// network without a person in the room.
func (e *Engine) WifiAutoConnect(ctx context.Context) error {
	e.mu.Lock()
	b := e.wifi
	if b == nil {
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()
	if err := b.AutoConnect(ctx); err != nil {
		return err
	}
	info, err := b.Info(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.wifiInfo = info
	e.mu.Unlock()
	return nil
}

// shortErr keeps user-facing errors to one line.
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	for i, r := range s {
		if r == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}
