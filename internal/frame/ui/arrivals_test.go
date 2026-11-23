package ui

import (
	"testing"
	"time"
)

// Arrival debuts: next-slot jump, ribbon lifecycle, caps, holds, and
// the ribbon's own repaint driver (design-review P1: with Clock off
// nothing else re-renders, so the ribbon must force its own frames).
func TestArrivalDebutFlow(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	p1 := Photo{Hash: repeat('a', 64), Name: "p1.jpg", DisplayPath: "/tmp/none"}
	p2 := Photo{Hash: repeat('b', 64), Name: "p2.jpg", Uploader: "Sarah", DisplayPath: "/tmp/none"}
	env := newUIEnv(t, []Photo{p1, p2}, nil)
	e := env.eng
	env.nowAt(now)
	e.Reload(t.Context())

	e.QueueArrivals([]string{p2.Hash})
	if len(e.debuts) != 1 || e.ribbon != nil {
		t.Fatalf("queue state: debuts=%v ribbon=%v", e.debuts, e.ribbon)
	}

	// Next forward advance debuts it regardless of rotation distance.
	e.advance(1) // p1 -> p2 (also the natural position)
	if e.ribbon == nil || e.ribbon.text != "from Sarah" {
		t.Fatalf("ribbon = %+v, want armed 'from Sarah'", e.ribbon)
	}
	if len(e.debuts) != 0 {
		t.Errorf("queue not consumed: %v", e.debuts)
	}

	// Countdown under held conditions (controls up): time freezes.
	// lastActivity kept fresh so the controls auto-hide rule (a
	// separate behavior) does not clear the hold mid-assertion.
	e.controls = true
	for i := 0; i < 3; i++ {
		now = now.Add(time.Second)
		e.lastActivity = now
		e.Tick(now)
	}
	if e.ribbon == nil || e.ribbon.left != ribbonShow {
		t.Fatalf("ribbon counted while controls open: %+v", e.ribbon)
	}

	// Unheld ticks burn the 8 s; ribbon clears at expiry.
	e.controls = false
	for i := 0; i < 9; i++ {
		now = now.Add(time.Second)
		e.Tick(now)
	}
	if e.ribbon != nil {
		t.Fatalf("ribbon outlived its deadline: %+v", e.ribbon)
	}
}

func TestDebutJumpsAhead(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	ps := make([]Photo, 5)
	for i := range ps {
		ps[i] = Photo{Hash: repeat(rune('a'+i), 64), Name: ps[i].Name, DisplayPath: "/tmp/none"}
		ps[i].Uploader = "Sam"
	}
	env := newUIEnv(t, ps, nil)
	e := env.eng
	env.nowAt(now)
	e.Reload(t.Context())

	// Debut target three slots ahead: the very next advance lands on
	// it (next-slot debut, not wait-three-intervals debut).
	e.QueueArrivals([]string{ps[3].Hash})
	e.advance(1)
	if e.idx != 3 || e.ribbon == nil || e.ribbon.text != "from Sam" {
		t.Fatalf("idx=%d ribbon=%+v, want jump to 3 with ribbon", e.idx, e.ribbon)
	}
	// Advancing away spends the ribbon.
	e.advance(1)
	if e.ribbon != nil {
		t.Errorf("ribbon survived leaving the debut photo")
	}
}

func TestDebutCapsAndCleanup(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	photos := []Photo{
		{Hash: repeat('a', 64), DisplayPath: "/tmp/none"},
		{Hash: repeat('b', 64), DisplayPath: "/tmp/none"},
		{Hash: repeat('c', 64), DisplayPath: "/tmp/none"},
		{Hash: repeat('d', 64), DisplayPath: "/tmp/none"},
		{Hash: repeat('e', 64), DisplayPath: "/tmp/none"},
		{Hash: repeat('f', 64), DisplayPath: "/tmp/none"},
	}
	env := newUIEnv(t, photos, nil)
	e := env.eng
	env.nowAt(now)
	e.Reload(t.Context())

	// R9: five arrivals queue at most three debuts.
	hashes := []string{photos[1].Hash, photos[2].Hash, photos[3].Hash, photos[4].Hash, photos[5].Hash}
	e.QueueArrivals(hashes)
	if len(e.debuts) != maxDebuts {
		t.Fatalf("queue = %d, want cap %d", len(e.debuts), maxDebuts)
	}

	// Three advances debut three photos (each armed, each spent).
	for i := 0; i < maxDebuts; i++ {
		e.advance(1)
		if e.ribbon == nil {
			t.Fatalf("advance %d: no ribbon armed", i)
		}
		now = now.Add(ribbonShow + time.Second)
		e.Tick(now) // expires it
	}
	if len(e.debuts) != 0 {
		t.Errorf("queue not drained: %v", e.debuts)
	}

	// Unknown hashes are dropped when they surface, never debut.
	e.QueueArrivals([]string{"ffffffff"})
	e.advance(1)
	if e.ribbon != nil || len(e.debuts) != 0 {
		t.Errorf("unknown hash: ribbon=%v queue=%v", e.ribbon, e.debuts)
	}
}

func TestRibbonDrivesOwnRepaintWhenClockOff(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	p1 := Photo{Hash: repeat('a', 64), DisplayPath: "/tmp/none"}
	p2 := Photo{Hash: repeat('b', 64), Uploader: "Jo", DisplayPath: "/tmp/none"}
	env := newUIEnv(t, []Photo{p1, p2}, nil)
	e := env.eng
	e.cfg.ShowClock = false
	env.nowAt(now)
	e.Reload(t.Context())

	e.QueueArrivals([]string{p2.Hash})
	e.advance(1) // arms the ribbon, presents once
	base := env.drv.Presents()
	for i := 0; i < 4; i++ {
		now = now.Add(time.Second)
		e.Tick(now)
	}
	if env.drv.Presents() < base+3 {
		t.Errorf("presents %d -> %d over 4 held ticks; ribbon must drive its own repaint with Clock off", base, env.drv.Presents())
	}
}

func repeat(r rune, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r)
	}
	return string(b)
}
