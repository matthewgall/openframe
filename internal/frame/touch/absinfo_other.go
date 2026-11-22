//go:build !linux

package touch

import "os"

// axisRange is unsupported off Linux: samples pass through
// uncalibrated (the frame's evdev path only matters on Linux
// hardware; dev boxes use the FIFO DevInput seam).
func axisRange(f *os.File, code int) (min, max int, ok bool) {
	return 0, 0, false
}
