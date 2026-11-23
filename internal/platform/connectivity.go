package platform

import (
	"os"
	"path/filepath"
	"strings"
)

// Link is the frame's physical network attachment, derived from
// kernel-reported interface state.
type Link int

const (
	LinkNone  Link = iota // no carrier on any interface
	LinkWired             // an ethernet interface is up with carrier
	LinkWiFi              // a wireless interface is up with carrier
)

// Connectivity reports the active link type from sysfs.
func Connectivity() Link { return ConnectivityIn("/sys") }

// ConnectivityIn reads <sysRoot>/class/net/<if>/{carrier,operstate}.
// Wired wins when both are present (an ethernet cable outranks Wi-Fi
// for the icon). Testable against a fake tree; interfaces named
// "lo" are ignored. Files missing or unreadable simply don't count
// (a down interface reads carrier=0 or operstate!="up").
func ConnectivityIn(sysRoot string) Link {
	dir := filepath.Join(sysRoot, "class", "net")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return LinkNone
	}
	link := LinkNone
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		up := ifaceUp(filepath.Join(dir, name))
		if !up {
			continue
		}
		switch {
		case strings.HasPrefix(name, "eth") || strings.HasPrefix(name, "en"):
			return LinkWired // wired dominates immediately
		case strings.HasPrefix(name, "wlan") || strings.HasPrefix(name, "wl"):
			link = LinkWiFi
		}
	}
	return link
}

func ifaceUp(path string) bool {
	carrier, err := os.ReadFile(filepath.Join(path, "carrier"))
	if err == nil && strings.TrimSpace(string(carrier)) == "1" {
		return true
	}
	// Some drivers expose no carrier file; fall back to operstate.
	state, err := os.ReadFile(filepath.Join(path, "operstate"))
	if err == nil {
		s := strings.TrimSpace(string(state))
		return s == "up"
	}
	return false
}
