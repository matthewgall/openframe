// Package sleep implements the display sleep schedule (spec §11:
// "Support configurable sleep schedules", default on 07:00 / off
// 22:00). The frame agent keeps running while the display is asleep;
// only the backlight follows the schedule.
//
// The schedule is a single daily window: the display is ON from On
// until Off, and OFF (asleep) from Off around through On. The window
// may cross midnight (e.g. off 22:00, on 07:00).
package sleep

import (
	"fmt"
	"strings"
	"time"
)

// Schedule is a daily on/off pair in minutes from midnight.
type Schedule struct {
	OnMin  int // e.g. 7*60+0 = 420
	OffMin int // e.g. 22*60+0 = 1320
	TZ     *time.Location
}

// FromConfig parses "HH:MM" strings and a time zone name into a
// Schedule. An empty time zone means the local zone. When BOTH on and
// off are empty the sleep schedule is disabled (empty window: the
// display never turns itself off); one empty and the other set is an
// error (ambiguous).
func FromConfig(on, off, tz string) (Schedule, error) {
	loc := time.Local
	if tz != "" {
		var err error
		loc, err = time.LoadLocation(tz)
		if err != nil {
			return Schedule{}, fmt.Errorf("sleep tz %q: %w", tz, err)
		}
	}
	if on == "" && off == "" {
		return Schedule{OnMin: 0, OffMin: 0, TZ: loc}, nil
	}
	if on == "" || off == "" {
		return Schedule{}, fmt.Errorf("sleep on and off must be set together")
	}
	onMin, err := parseHM(on)
	if err != nil {
		return Schedule{}, fmt.Errorf("sleep on %q: %w", on, err)
	}
	offMin, err := parseHM(off)
	if err != nil {
		return Schedule{}, fmt.Errorf("sleep off %q: %w", off, err)
	}
	return Schedule{OnMin: onMin, OffMin: offMin, TZ: loc}, nil
}

func parseHM(s string) (int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("want HH:MM")
	}
	var h, m int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return 0, err
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return 0, err
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("out of range")
	}
	return h*60 + m, nil
}

// IsAsleep reports whether the display should be OFF at t. The asleep
// window runs from OffMin until OnMin, wrapping past midnight when
// OffMin > OnMin (the normal night case).
//
// Edge semantics: at exactly OffMin the display turns off; at exactly
// OnMin it turns on. When OnMin == OffMin the display is always on
// (an empty sleep window), which is the safe default.
func (s Schedule) IsAsleep(t time.Time) bool {
	if s.OnMin == s.OffMin {
		return false
	}
	local := t.In(s.TZ)
	nowMin := local.Hour()*60 + local.Minute()
	if s.OffMin > s.OnMin {
		// Normal: off in the evening, on in the morning.
		return nowMin >= s.OffMin || nowMin < s.OnMin
	}
	// Inverted (off before on, e.g. off 02:00 on 09:00): asleep from
	// off in the early morning through to on later the same day.
	return nowMin >= s.OffMin && nowMin < s.OnMin
}

// Description renders the schedule for the settings screen.
func (s Schedule) Description() string {
	return fmt.Sprintf("%02d:%02d – %02d:%02d",
		s.OnMin/60, s.OnMin%60, s.OffMin/60, s.OffMin%60)
}
