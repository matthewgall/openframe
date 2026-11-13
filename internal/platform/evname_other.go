//go:build !linux

package platform

// readInputName returns "" off-Linux: evdev is a Linux interface
// (see evname_linux.go).
func readInputName(path string) string { return "" }
