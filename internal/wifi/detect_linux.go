//go:build linux

package wifi

import (
	"context"
	"errors"
)

// detect picks the best backend: NetworkManager first (Raspberry Pi
// OS ships it and persists connections itself), then raw
// wpa_supplicant, then "interface present but no tooling".
func detect(root string, look func(string) (string, error)) Backend {
	ifname, ok := findWifiIf(root)
	if !ok {
		return &unavailableBackend{}
	}
	if _, err := look("nmcli"); err == nil && nmcliHasWifi() {
		return &nmcliBackend{ifname: ifname, root: root}
	}
	if _, err := look("wpa_supplicant"); err == nil {
		return &supplicantBackend{
			ifname:   ifname,
			root:     root,
			confPath: defaultConfPath(),
		}
	}
	return &limitedBackend{ifname: ifname, root: root}
}

func defaultConfPath() string { return "/etc/wpa_supplicant/openframe.conf" }

// limitedBackend: the radio exists but neither NetworkManager nor
// wpa_supplicant is installed. State is reported; actions explain
// what is missing.
type limitedBackend struct {
	ifname string
	root   string
}

func (l *limitedBackend) Info(ctx context.Context) (Info, error) {
	return Info{
		Available: true,
		Backend:   "none",
		Interface: l.ifname,
		WiredUp:   wiredUp(l.root),
	}, nil
}

var errNoTooling = errors.New("no Wi-Fi tooling installed (need NetworkManager or wpa_supplicant)")

func (l *limitedBackend) Scan(context.Context) ([]Network, error) {
	return nil, errNoTooling
}
func (l *limitedBackend) Connect(context.Context, string, string) error {
	return errNoTooling
}
func (l *limitedBackend) Forget(context.Context, string) error {
	return errNoTooling
}
func (l *limitedBackend) AutoConnect(context.Context) error {
	return nil
}
