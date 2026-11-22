//go:build linux

package touch

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// absInfo mirrors struct input_absinfo (linux/input.h): six int32
// fields, value/min/max/fuzz/flat/resolution.
type absInfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

// eviocgabs is _IOR('E', 0x40 + code, struct input_absinfo).
// _IOC_READ is 2 (not 1!), so the dir bits are 2<<30 = 0x80000000;
// size 24 (6 x int32) at bits 16..29, type 'E' at 8..15, nr base 0x40.
const eviocgabs = 0x80184540

// axisRange returns the raw min/max of one EV_ABS code. ok=false when
// the kernel rejects the query (device has no such axis).
func axisRange(f *os.File, code int) (min, max int, ok bool) {
	var ai absInfo
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), eviocgabs+uintptr(code), uintptr(unsafe.Pointer(&ai)))
	if errno != 0 {
		return 0, 0, false
	}
	return int(ai.Minimum), int(ai.Maximum), true
}
