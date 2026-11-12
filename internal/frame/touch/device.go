package touch

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Stream reads the device until ctx is cancelled, emitting one Sample
// per finger position on ch. The channel is closed on ctx cancel or
// an unrecoverable read error. A goroutine is started; callers stop it
// via ctx.
func (d *Device) Stream(ctx context.Context) <-chan Sample {
	ch := make(chan Sample, 64)
	go func() {
		defer close(ch)
		for {
			evs, err := d.readFrame()
			if err != nil {
				return
			}
			samples := SampleFrames(evs, time.Now)
			for _, s := range samples {
				select {
				case ch <- s:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
	return ch
}

// FindTouchscreen scans sysRoot/class/input/event* for a device whose
// name looks like a touchscreen and returns its /dev/input/eventN
// node. sysRoot defaults to "/sys" and devRoot to "/dev"; both are
// parameters so the scan is unit-testable against a fake tree.
//
// The heuristic matches the common capacitive-panel names shipped on
// Raspberry Pi frame hardware (Goodix, FT5x06, generic "touchscreen"
// / "Touch" / "TS" names). A USB mouse or keyboard will not match.
func FindTouchscreen(sysRoot, devRoot string) (string, error) {
	if sysRoot == "" {
		sysRoot = "/sys"
	}
	if devRoot == "" {
		devRoot = "/dev"
	}
	entries, err := os.ReadDir(filepath.Join(sysRoot, "class", "input"))
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "event") {
			continue
		}
		name := readInputName(filepath.Join(sysRoot, "class", "input", e.Name()))
		if isTouchscreenName(name) {
			candidates = append(candidates, e.Name())
		}
	}
	if len(candidates) == 0 {
		return "", os.ErrNotExist
	}
	sort.Strings(candidates) // deterministic: lowest eventN wins
	node := filepath.Join(devRoot, "input", candidates[0])
	if _, err := os.Stat(node); err != nil {
		return "", err
	}
	return node, nil
}

// readInputName reads the device name for an event node. It tries the
// sysfs "name" file, then the uevent NAME= fallback.
func readInputName(eventDir string) string {
	if b, err := os.ReadFile(filepath.Join(eventDir, "device", "name")); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	if b, err := os.ReadFile(filepath.Join(eventDir, "device", "uevent")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "NAME="); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// isTouchscreenName reports whether a device name looks like a
// capacitive or resistive touchscreen.
func isTouchscreenName(name string) bool {
	n := strings.ToLower(name)
	// Order matters: reject obvious non-touch names first.
	for _, bad := range []string{"mouse", "keyboard", "keypad", "joystick", "gamepad", "touchpad"} {
		if strings.Contains(n, bad) {
			return false
		}
	}
	for _, good := range []string{
		"touchscreen", "touch screen", "touchscreen",
		"goodix", "gt9", "gt1151", "ft5", "ft6", "i2c-touch",
		"capacitive", " resistive ", "ts_", "touch",
	} {
		if strings.Contains(n, good) {
			return true
		}
	}
	return false
}
