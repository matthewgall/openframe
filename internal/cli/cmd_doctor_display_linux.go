//go:build linux

package cli

import (
	"github.com/matthewgall/openframe/internal/frame/display/drm"
)

// checkDisplay probes every DRM card through the same code path the
// frame uses, so doctor reports what the frame will actually see.
func checkDisplay(add func(string, checkStatus, string)) {
	dispDetails := []string{}
	panelFound := false
	for _, card := range drm.ScanCards() {
		mode, err := drm.Available(card)
		if err != nil {
			dispDetails = append(dispDetails, card+": no connected panel ("+shortErr(err)+")")
			continue
		}
		panelFound = true
		dispDetails = append(dispDetails, card+": "+mode)
	}
	if panelFound {
		add("display", checkOK, joinDetails(dispDetails))
	} else if len(dispDetails) > 0 {
		add("display", checkWarn, joinDetails(dispDetails)+" — the headless dev renderer will be used")
	} else {
		add("display", checkWarn, "no /dev/dri card found; the headless dev renderer will be used")
	}
}
