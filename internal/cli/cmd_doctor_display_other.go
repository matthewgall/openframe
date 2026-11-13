//go:build !linux

package cli

// checkDisplay: the DRM/KMS backend is a Linux interface; on other
// platforms the frame runs the headless dev renderer.
func checkDisplay(add func(string, checkStatus, string)) {
	add("display", checkWarn, "DRM is a Linux interface; the headless dev renderer will be used on this platform")
}
