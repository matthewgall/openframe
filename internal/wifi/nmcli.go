package wifi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// nmcliBackend drives NetworkManager (Raspberry Pi OS). NM persists
// connections itself, so AutoConnect is a no-op and Forget uses
// `connection forget`.
type nmcliBackend struct {
	ifname string
	root   string // /sys/class/net (test seam)
}

// nmcliHasWifi reports whether NetworkManager sees a Wi-Fi device.
func nmcliHasWifi() bool {
	out, err := run(context.Background(), 5*time.Second, "nmcli", "-t", "-f", "TYPE,STATE", "dev")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "wifi:") {
			return true
		}
	}
	return false
}

func (n *nmcliBackend) Info(ctx context.Context) (Info, error) {
	info := Info{
		Available: true,
		Backend:   "networkmanager",
		Interface: n.ifname,
		WiredUp:   wiredUp(n.root),
	}
	// ACTIVE:SSID pairs; the active one (if any) carries the SSID.
	if out, err := run(ctx, 5*time.Second, "nmcli", "-t", "-f", "ACTIVE,SSID", "dev", "wifi"); err == nil {
		info.ConnectedSSID = nmcliActiveSSID(out)
	}
	return info, nil
}

// nmcliActiveSSID extracts the active Wi-Fi SSID from
// `nmcli -t -f ACTIVE,SSID dev wifi` output ("yes:SSID" lines).
func nmcliActiveSSID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && parts[0] == "yes" && parts[1] != "" {
			return parts[1]
		}
	}
	return ""
}

// parseNmcliScan parses `nmcli -t -f SSID,SIGNAL,SECURITY dev wifi
// list` output. Hidden networks (empty SSID) are skipped — they are
// reachable via manual entry.
func parseNmcliScan(out string) []Network {
	var nets []Network
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		sig, err := strconv.Atoi(parts[1])
		if err != nil {
			sig = 0
		}
		nets = append(nets, Network{
			SSID:      parts[0],
			Signal:    sig,
			Encrypted: len(parts) > 2 && parts[2] != "",
		})
	}
	return nets
}

func (n *nmcliBackend) Scan(ctx context.Context) ([]Network, error) {
	out, err := run(ctx, 10*time.Second, "nmcli", "-t", "-f", "SSID,SIGNAL,SECURITY", "dev", "wifi", "list")
	if err != nil {
		return nil, fmt.Errorf("wifi scan: %w", err)
	}
	return parseNmcliScan(out), nil
}

func (n *nmcliBackend) Connect(ctx context.Context, ssid, psk string) error {
	if ssid == "" {
		return fmt.Errorf("wifi: no network selected")
	}
	args := []string{"-a", "no", "device", "wifi", "connect", ssid}
	if psk != "" {
		args = append(args, "password", psk)
	}
	// Association + 4-way handshake + DHCP can take a while.
	out, err := run(ctx, 60*time.Second, "nmcli", args...)
	if err != nil {
		return fmt.Errorf("wifi connect: %v: %s", err, tail(out))
	}
	return nil
}

func (n *nmcliBackend) Forget(ctx context.Context, ssid string) error {
	_, err := run(ctx, 10*time.Second, "nmcli", "-a", "no", "connection", "forget", ssid)
	if err != nil {
		return fmt.Errorf("wifi forget: %v: %s", err, tail(err.Error()))
	}
	return nil
}

func (n *nmcliBackend) AutoConnect(context.Context) error { return nil }

// tail keeps user-facing errors short (no full command transcripts,
// and nothing that could carry a passphrase — connect errors from
// nmcli never echo the password, but stay defensive).
func tail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = "…" + s[len(s)-160:]
	}
	return s
}
