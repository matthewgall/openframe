// Package thermal implements thermal throttling (spec §7 "hardware:
// thermal throttling"). When the platform's temperature exceeds a
// threshold, the frame slows its slideshow (doubled interval) to shed
// sustained load and let the silicon cool.
//
// The reader is an interface so the sysfs implementation is testable
// against a fake directory, and the monitor decision is a pure
// function of the last reading.
package thermal

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Reader reports the platform's current maximum temperature in °C.
type Reader interface {
	MaxTempC() (float64, error)
}

// SysfsReader reads /sys/class/thermal/thermal_zone*/temp (milli-°C).
// dir defaults to /sys/class/thermal.
type SysfsReader struct {
	dir string
}

// NewSysfsReader builds a reader over dir (empty = /sys/class/thermal).
func NewSysfsReader(dir string) *SysfsReader {
	if dir == "" {
		dir = "/sys/class/thermal"
	}
	return &SysfsReader{dir: dir}
}

// MaxTempC returns the highest temperature across all thermal zones.
// Zones whose temp file is unreadable are skipped. An error is returned
// only if no zone could be read at all.
func (r *SysfsReader) MaxTempC() (float64, error) {
	zones, err := os.ReadDir(r.dir)
	if err != nil {
		return 0, fmt.Errorf("thermal: %w", err)
	}
	var max float64
	found := false
	for _, z := range zones {
		if !strings.HasPrefix(z.Name(), "thermal_zone") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(r.dir, z.Name(), "temp"))
		if err != nil {
			continue
		}
		milli, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			continue
		}
		c := float64(milli) / 1000.0
		if !found || c > max {
			max = c
			found = true
		}
	}
	if !found {
		return 0, fmt.Errorf("thermal: no readable thermal zones in %s", r.dir)
	}
	return max, nil
}

// Monitor wraps a Reader and a threshold.
type Monitor struct {
	Reader
	ThresholdC float64
}

// Throttled reports whether the last reading exceeds the threshold.
// A read error is treated as "not throttled" (fail open): we cannot
// confirm overheating, and the slideshow must not stop for a missing
// sensor.
func (m *Monitor) Throttled() bool {
	if m.ThresholdC <= 0 {
		return false
	}
	c, err := m.Reader.MaxTempC()
	if err != nil {
		return false
	}
	return c >= m.ThresholdC
}

// NewMonitor builds a Monitor. Enabled=false yields a Monitor that
// never throttles.
func NewMonitor(enabled bool, reader Reader, thresholdC float64) *Monitor {
	if !enabled {
		return &Monitor{Reader: reader, ThresholdC: -1}
	}
	return &Monitor{Reader: reader, ThresholdC: thresholdC}
}
