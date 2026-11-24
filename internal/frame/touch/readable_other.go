//go:build !linux

package touch

import "time"

// waitReadable is a pass-through off Linux: there are no real evdev
// nodes here, only test pipes, and the platforms' poll APIs are not
// worth a second implementation. The Linux build carries the
// cancellable-wait behaviour that production shutdown depends on.
func (d *Device) waitReadable(dur time.Duration) bool { return true }
