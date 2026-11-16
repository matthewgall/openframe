package cli

import (
	"context"
	"fmt"

	wifipkg "github.com/matthewgall/openframe/internal/wifi"
)

// wifiCheck reports what the frame's on-screen Wi-Fi setup (ADR-0014)
// would find: the radio, the management backend, and current state.
func wifiCheck(ctx context.Context, add func(string, checkStatus, string)) {
	info, err := wifipkg.New().Info(ctx)
	if err != nil || !info.Available {
		add("wifi", checkWarn, "no wireless interface — the frame must use wired networking")
		return
	}
	detail := fmt.Sprintf("%s via %s", info.Interface, info.Backend)
	switch {
	case info.WiredUp:
		detail += " (wired up)"
	case info.ConnectedSSID != "":
		detail += " (connected: " + info.ConnectedSSID + ")"
	default:
		detail += " (not connected)"
	}
	if info.Backend == "none" {
		add("wifi", checkWarn, detail+" — install NetworkManager or wpa_supplicant (+ iw) to enable on-screen setup")
		return
	}
	add("wifi", checkOK, detail)
}
