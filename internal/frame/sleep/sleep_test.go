package sleep

import (
	"testing"
	"time"
)

func at(h, m int) time.Time {
	// Fixed zone (UTC) so DST can't interfere.
	return time.Date(2026, 6, 15, h, m, 0, 0, time.UTC)
}

func TestIsAsleepNormalWindow(t *testing.T) {
	// Off 22:00, on 07:00 (the spec default).
	s := Schedule{OnMin: 7 * 60, OffMin: 22 * 60, TZ: time.UTC}
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at(0, 0), true},  // 00:00 asleep
		{at(6, 59), true}, // just before on
		{at(7, 0), false}, // exactly on -> awake
		{at(12, 0), false},
		{at(21, 59), false}, // just before off
		{at(22, 0), true},   // exactly off -> asleep
		{at(23, 30), true},
	}
	for _, tc := range cases {
		if got := s.IsAsleep(tc.t); got != tc.want {
			t.Errorf("IsAsleep(%s) = %v, want %v", tc.t.Format("15:04"), got, tc.want)
		}
	}
}

func TestIsAsleepInvertedWindow(t *testing.T) {
	// Off 02:00, on 09:00: asleep 02:00-09:00, on the rest of the day.
	s := Schedule{OnMin: 9 * 60, OffMin: 2 * 60, TZ: time.UTC}
	if s.IsAsleep(at(1, 59)) {
		t.Error("01:59 should be awake")
	}
	if !s.IsAsleep(at(2, 0)) {
		t.Error("02:00 should be asleep")
	}
	if !s.IsAsleep(at(8, 59)) {
		t.Error("08:59 should be asleep")
	}
	if s.IsAsleep(at(9, 0)) {
		t.Error("09:00 should be awake")
	}
	if s.IsAsleep(at(23, 0)) {
		t.Error("23:00 should be awake")
	}
}

func TestIsAsleepEmptyWindow(t *testing.T) {
	s := Schedule{OnMin: 12 * 60, OffMin: 12 * 60, TZ: time.UTC}
	if s.IsAsleep(at(3, 0)) {
		t.Error("empty window must never sleep")
	}
}

func TestFromConfig(t *testing.T) {
	s, err := FromConfig("07:00", "22:00", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if s.OnMin != 420 || s.OffMin != 1320 {
		t.Errorf("parsed %+v", s)
	}
	if s.Description() != "07:00 – 22:00" {
		t.Errorf("desc = %q", s.Description())
	}
	for _, bad := range []struct{ on, off, tz string }{
		{"7:00", "22:00", ""}, // single-digit hour ok
		{"07:60", "22:00", ""},
		{"24:00", "22:00", ""},
		{"07:00", "22:00", "No/Zone"},
	} {
		_, err := FromConfig(bad.on, bad.off, bad.tz)
		wantErr := bad.on == "07:60" || bad.on == "24:00" || bad.tz == "No/Zone"
		if (err != nil) != wantErr {
			t.Errorf("FromConfig(%q,%q,%q) err=%v, wantErr=%v", bad.on, bad.off, bad.tz, err, wantErr)
		}
	}
}
