//go:build linux

package touch

import (
	"time"

	"golang.org/x/sys/unix"
)

// waitReadable reports whether the device fd has data — or went away
// (returning true then lets the next read surface the real error).
// False means the poll window expired with nothing to read: the
// caller re-checks its context and polls again, which is what makes
// an idle touchscreen's zero-event silence cancellable.
func (d *Device) waitReadable(dur time.Duration) bool {
	ms := int(dur / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	fds := []unix.PollFd{{Fd: int32(d.f.Fd()), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, ms)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return true
		}
		return n > 0
	}
}
