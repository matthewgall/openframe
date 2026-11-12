package thermal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeReader struct {
	c   float64
	err error
}

func (f fakeReader) MaxTempC() (float64, error) { return f.c, f.err }

func TestMonitorThreshold(t *testing.T) {
	m := NewMonitor(true, fakeReader{c: 69.9}, 70)
	if m.Throttled() {
		t.Error("69.9C < 70C should not throttle")
	}
	m = NewMonitor(true, fakeReader{c: 70.0}, 70)
	if !m.Throttled() {
		t.Error("70.0C >= 70C should throttle")
	}
	m = NewMonitor(true, fakeReader{c: 95}, 70)
	if !m.Throttled() {
		t.Error("95C should throttle")
	}
}

func TestMonitorFailOpenOnError(t *testing.T) {
	m := NewMonitor(true, fakeReader{err: errors.New("no sensor")}, 70)
	if m.Throttled() {
		t.Error("read error must fail open (not throttle)")
	}
}

func TestMonitorDisabled(t *testing.T) {
	m := NewMonitor(false, fakeReader{c: 120}, 70)
	if m.Throttled() {
		t.Error("disabled monitor must never throttle")
	}
}

func TestSysfsReader(t *testing.T) {
	dir := t.TempDir()
	mk := func(zone string, milli string) {
		if err := os.MkdirAll(filepath.Join(dir, zone), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, zone, "temp"), []byte(milli), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("thermal_zone0", "55000") // 55.0C
	mk("thermal_zone1", "62500") // 62.5C
	// A zone without a temp file is skipped.
	if err := os.MkdirAll(filepath.Join(dir, "thermal_zone2"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewSysfsReader(dir)
	c, err := r.MaxTempC()
	if err != nil {
		t.Fatal(err)
	}
	if c != 62.5 {
		t.Errorf("max = %vC, want 62.5C", c)
	}
}

func TestSysfsReaderNoZones(t *testing.T) {
	dir := t.TempDir()
	r := NewSysfsReader(dir)
	if _, err := r.MaxTempC(); err == nil {
		t.Error("expected error with no zones")
	}
}
