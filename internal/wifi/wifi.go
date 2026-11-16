// Package wifi is the frame's Wi-Fi seam (ADR-0014): it reports
// connectivity state, scans networks and connects — on Linux through
// NetworkManager (preferred) or wpa_supplicant (fallback), chosen by
// autodetection at startup. Other platforms report "unavailable" so
// the UI can hide the feature.
//
// Security rules (ADR-0014): a passphrase is never logged, never sent
// to the server and never written outside a 0600 config file. All
// commands run via exec with argument lists — never a shell.
package wifi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Info describes the current connectivity state.
type Info struct {
	Available     bool   // a wireless interface exists
	Backend       string // "networkmanager" | "wpa_supplicant" | "none"
	Interface     string // e.g. "wlan0" ("" if none)
	WiredUp       bool   // a non-wireless interface is operstate "up"
	ConnectedSSID string // "" = not connected over Wi-Fi
}

// Network is one scan result.
type Network struct {
	SSID      string
	Signal    int // 0..100
	Encrypted bool
}

// Backend is the capability the frame UI needs. Implementations:
// nmcli (NetworkManager), supplicant (wpa_supplicant + iw), limited
// (interface present, no tooling), unavailable (no interface).
type Backend interface {
	Info(ctx context.Context) (Info, error)
	Scan(ctx context.Context) ([]Network, error)
	// Connect associates with ssid and, if psk is non-empty,
	// authenticates with it. Empty psk = open network.
	Connect(ctx context.Context, ssid, psk string) error
	// Forget removes ssid from the saved networks so the frame does
	// not auto-reconnect to it.
	Forget(ctx context.Context, ssid string) error
	// AutoConnect resumes saved networks on boot (a no-op where the
	// tooling persists connections itself).
	AutoConnect(ctx context.Context) error
}

// New autodetects the best backend (Linux) or returns the unavailable
// backend elsewhere. Detection is cheap and cached for the process.
func New() Backend {
	b := detect(sysClassNetRoot(), exec.LookPath)
	if b, ok := b.(*unavailableBackend); ok {
		return b
	}
	return &wrapped{b: b}
}

// wrapped adds a short Info TTL so the UI can call Info() on every
// render without spawning a process each time.
type wrapped struct {
	b Backend

	mu     sync.Mutex
	cached Info
	at     time.Time
	ok     bool
}

const infoTTL = 5 * time.Second

func (w *wrapped) Scan(ctx context.Context) ([]Network, error)    { return w.b.Scan(ctx) }
func (w *wrapped) Connect(ctx context.Context, s, p string) error { return w.b.Connect(ctx, s, p) }
func (w *wrapped) Forget(ctx context.Context, s string) error     { return w.b.Forget(ctx, s) }
func (w *wrapped) AutoConnect(ctx context.Context) error          { return w.b.AutoConnect(ctx) }

func (w *wrapped) Info(ctx context.Context) (Info, error) {
	w.mu.Lock()
	if w.ok && time.Since(w.at) < infoTTL {
		i := w.cached
		w.mu.Unlock()
		return i, nil
	}
	w.mu.Unlock()
	i, err := w.b.Info(ctx)
	if err == nil {
		w.mu.Lock()
		w.cached, w.at, w.ok = i, time.Now(), true
		w.mu.Unlock()
	}
	return i, err
}

// sysClassNetRoot returns the /sys/class/net root (a parameter so
// detection is testable against a fake tree).
func sysClassNetRoot() string { return "/sys/class/net" }

// findWifiIf returns the first (name-sorted) wireless interface under
// root, where "wireless" means the interface has a <root>/<if>/
// wireless sysfs directory.
func findWifiIf(root string) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	var names []string
	for _, e := range entries {
		if e.Name() == "lo" {
			continue
		}
		if _, err := os.Stat(root + "/" + e.Name() + "/wireless"); err == nil {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return names[0], true
}

// wiredUp reports whether any non-wireless, non-lo interface under
// root is operstate "up".
func wiredUp(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == "lo" {
			continue
		}
		if _, err := os.Stat(root + "/" + e.Name() + "/wireless"); err == nil {
			continue // wireless interface, not "wired"
		}
		b, err := os.ReadFile(root + "/" + e.Name() + "/operstate")
		if err == nil && strings.TrimSpace(string(b)) == "up" {
			return true
		}
	}
	return false
}

// run captures a command's trimmed output.
func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// dbmToPercent maps a signal level to a 0..100 percentage on the
// common -30..-90 dBm scale.
func dbmToPercent(dbm int) int {
	pct := (dbm + 90) * 100 / 60
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// unavailableBackend is the no-hardware / non-Linux backend.
type unavailableBackend struct{}

func (u *unavailableBackend) Info(context.Context) (Info, error) {
	return Info{Backend: "none"}, nil
}

func (u *unavailableBackend) Scan(context.Context) ([]Network, error) {
	return nil, errNoWifi
}
func (u *unavailableBackend) Connect(context.Context, string, string) error {
	return errNoWifi
}
func (u *unavailableBackend) Forget(context.Context, string) error {
	return errNoWifi
}
func (u *unavailableBackend) AutoConnect(context.Context) error { return nil }

var errNoWifi = errors.New("Wi-Fi is not available on this system")
