//go:build linux

package platform

import (
	"os"
	"strings"
	"syscall"
)

// readInputName returns the human-readable name of an evdev device, or ""
// if it cannot be read (permissions). It uses the EVIOCGNAME ioctl.
func readInputName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	const eviocgname = 0x80184600 // EVIOCGNAME(32)
	buf := make([]byte, 32)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), eviocgname, uintptr(uintptr(len(buf))))
	_ = errno
	name := strings.TrimRight(string(buf), "\x00")
	return name
}
