package touch

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeEvent builds a 24-byte input_event record (timeval zeroed).
func makeEvent(typ, code, val int) []byte {
	b := make([]byte, eventSize)
	binary.LittleEndian.PutUint16(b[16:], uint16(typ))
	binary.LittleEndian.PutUint16(b[18:], uint16(code))
	binary.LittleEndian.PutUint32(b[20:], uint32(val))
	return b
}

func TestParseEvents(t *testing.T) {
	buf := make([]byte, 0)
	buf = append(buf, makeEvent(evAbs, absX, 123)...)
	buf = append(buf, makeEvent(evAbs, absY, 456)...)
	buf = append(buf, makeEvent(evKey, btnTouch, 1)...)
	buf = append(buf, makeEvent(evSyn, synReport, 0)...)
	// A trailing partial record must be ignored.
	buf = append(buf, makeEvent(evAbs, absX, 9)[:10]...)

	evs := ParseEvents(buf)
	if len(evs) != 4 {
		t.Fatalf("parsed %d events, want 4", len(evs))
	}
	want := []rawEvent{
		{evAbs, absX, 123},
		{evAbs, absY, 456},
		{evKey, btnTouch, 1},
		{evSyn, synReport, 0},
	}
	for i := range want {
		if evs[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, evs[i], want[i])
		}
	}
}

func TestSampleFrames(t *testing.T) {
	// Frame 1: touch down at (10, 20). Frame 2: move to (15, 22).
	// Frame 3: touch up at (15, 22).
	evs := []rawEvent{
		{evKey, btnTouch, 1}, {evAbs, absX, 10}, {evAbs, absY, 20}, {evSyn, synReport, 0},
		{evAbs, absX, 15}, {evAbs, absY, 22}, {evSyn, synReport, 0},
		{evKey, btnTouch, 0}, {evSyn, synReport, 0},
	}
	base := time.Now()
	i := 0
	samples := SampleFrames(evs, func() time.Time {
		defer func() { i++ }()
		return base.Add(time.Duration(i) * time.Millisecond)
	})
	if len(samples) != 3 {
		t.Fatalf("got %d samples: %+v", len(samples), samples)
	}
	if !samples[0].Down || samples[0].X != 10 || samples[0].Y != 20 {
		t.Errorf("down sample = %+v", samples[0])
	}
	if samples[1].Down || samples[1].Up || samples[1].X != 15 || samples[1].Y != 22 {
		t.Errorf("move sample = %+v", samples[1])
	}
	if !samples[2].Up || samples[2].Down {
		t.Errorf("up sample = %+v", samples[2])
	}
}

func mustGesture(t *testing.T, evs []rawEvent, mk func() time.Time) []Gesture {
	t.Helper()
	r := NewRecognizer(Options{})
	var out []Gesture
	for _, s := range SampleFrames(evs, mk) {
		out = append(out, r.Feed(s)...)
	}
	return out
}

func tapEvs(down [2]int, up [2]int) []rawEvent {
	return []rawEvent{
		{evKey, btnTouch, 1}, {evAbs, absX, down[0]}, {evAbs, absY, down[1]}, {evSyn, synReport, 0},
		{evKey, btnTouch, 0}, {evAbs, absX, up[0]}, {evAbs, absY, up[1]}, {evSyn, synReport, 0},
	}
}

func TestGestureTap(t *testing.T) {
	base := time.Now()
	gs := mustGesture(t, tapEvs([2]int{100, 100}, [2]int{105, 102}),
		func() time.Time { return base })
	if len(gs) != 1 || gs[0].Kind != KindTap {
		t.Fatalf("gestures = %+v, want one tap", gs)
	}
	if gs[0].Pos != [2]int{105, 102} {
		t.Errorf("tap pos = %v", gs[0].Pos)
	}
}

func TestGestureDoubleTap(t *testing.T) {
	base := time.Now()
	var evs []rawEvent
	evs = append(evs, tapEvs([2]int{100, 100}, [2]int{102, 101})...)
	evs = append(evs, tapEvs([2]int{104, 99}, [2]int{103, 100})...)
	stamp := 0
	gs := mustGesture(t, evs, func() time.Time {
		defer func() { stamp++ }()
		// Two taps 150ms apart: first tap completes at 50ms, second
		// at 200ms.
		if stamp == 0 {
			return base.Add(50 * time.Millisecond)
		}
		return base.Add(200 * time.Millisecond)
	})
	if len(gs) != 2 || gs[0].Kind != KindTap || gs[1].Kind != KindDoubleTap {
		t.Fatalf("gestures = %+v, want tap then double-tap", gs)
	}
}

func TestGestureSwipeDirections(t *testing.T) {
	base := time.Now()
	cases := []struct {
		name string
		down [2]int
		up   [2]int
		dir  Dir
	}{
		{"left", [2]int{400, 100}, [2]int{100, 110}, DirLeft},
		{"right", [2]int{100, 100}, [2]int{400, 90}, DirRight},
		{"up", [2]int{100, 400}, [2]int{110, 100}, DirUp},
		{"down", [2]int{100, 100}, [2]int{90, 400}, DirDown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var evs []rawEvent
			evs = append(evs,
				rawEvent{evKey, btnTouch, 1}, rawEvent{evAbs, absX, tc.down[0]}, rawEvent{evAbs, absY, tc.down[1]}, rawEvent{evSyn, synReport, 0})
			evs = append(evs,
				rawEvent{evAbs, absX, tc.up[0]}, rawEvent{evAbs, absY, tc.up[1]}, rawEvent{evSyn, synReport, 0})
			evs = append(evs,
				rawEvent{evKey, btnTouch, 0}, rawEvent{evSyn, synReport, 0})
			gs := mustGesture(t, evs, func() time.Time { return base.Add(100 * time.Millisecond) })
			if len(gs) != 1 || gs[0].Kind != KindSwipe || gs[0].Dir != tc.dir {
				t.Fatalf("gestures = %+v, want swipe %s", gs, tc.dir)
			}
		})
	}
}

func TestGestureHold(t *testing.T) {
	base := time.Now()
	var evs []rawEvent
	evs = append(evs,
		rawEvent{evKey, btnTouch, 1}, rawEvent{evAbs, absX, 200}, rawEvent{evAbs, absY, 200}, rawEvent{evSyn, synReport, 0})
	evs = append(evs,
		rawEvent{evKey, btnTouch, 0}, rawEvent{evSyn, synReport, 0})
	stamp := 0
	gs := mustGesture(t, evs, func() time.Time {
		defer func() { stamp++ }()
		if stamp == 0 {
			return base
		}
		return base.Add(700 * time.Millisecond)
	})
	if len(gs) != 1 || gs[0].Kind != KindHold {
		t.Fatalf("gestures = %+v, want hold", gs)
	}
}

func TestGestureSmallSlowMoveIsNotSwipe(t *testing.T) {
	base := time.Now()
	// Moves 30px over 200ms: too short to swipe, too long for tap ->
	// no gesture (dead zone), which is the correct accessible
	// behaviour.
	var evs []rawEvent
	evs = append(evs,
		rawEvent{evKey, btnTouch, 1}, rawEvent{evAbs, absX, 100}, rawEvent{evAbs, absY, 100}, rawEvent{evSyn, synReport, 0})
	evs = append(evs,
		rawEvent{evAbs, absX, 130}, rawEvent{evAbs, absY, 100}, rawEvent{evSyn, synReport, 0})
	evs = append(evs,
		rawEvent{evKey, btnTouch, 0}, rawEvent{evSyn, synReport, 0})
	stamp := 0
	gs := mustGesture(t, evs, func() time.Time {
		defer func() { stamp++ }()
		if stamp == 0 {
			return base
		}
		return base.Add(200 * time.Millisecond)
	})
	if len(gs) != 0 {
		t.Fatalf("gestures = %+v, want none", gs)
	}
}

func TestBtnTouchMatchesKernelHeader(t *testing.T) {
	// BTN_TOUCH is 0x14a in linux/input-event-codes.h. The parser once
	// used 0x14b (BTN_TOUCH+1, BTN_HAND), which made every real
	// touchscreen invisible. Pin the constant to the header value.
	if btnTouch != 0x14a {
		t.Fatalf("btnTouch = 0x%x, kernel BTN_TOUCH is 0x14a", btnTouch)
	}
}

func TestSampleFramesMTOnly(t *testing.T) {
	// Type-B device: positions arrive on the MT axes only; the panel's
	// BTN_TOUCH mirrors contact state.
	evs := []rawEvent{
		{evAbs, absMTPositionX, 300}, {evAbs, absMTPositionY, 400},
		{evKey, btnTouch, 1}, {evSyn, synReport, 0},
		{evAbs, absMTPositionX, 310}, {evSyn, synReport, 0},
		{evKey, btnTouch, 0}, {evSyn, synReport, 0},
	}
	samples := SampleFrames(evs, func() time.Time { return time.Unix(0, 0) })
	if len(samples) != 3 {
		t.Fatalf("got %d samples, want 3", len(samples))
	}
	if samples[0].X != 300 || samples[0].Y != 400 || !samples[0].Down {
		t.Errorf("down sample = %+v", samples[0])
	}
	if samples[1].X != 310 || samples[1].Y != 400 {
		t.Errorf("move sample = %+v", samples[1])
	}
	if !samples[2].Up {
		t.Errorf("up sample = %+v", samples[2])
	}
}

func TestPositionTrackerAcrossFrames(t *testing.T) {
	// Real evdev delivery: every readFrame call returns one SYN-terminated
	// frame. The releasing frame carries BTN_TOUCH=0 and NO coordinates
	// (exactly what the HID 0712:000a panel sends). A per-call stateless
	// parse drops all movement and reports finger-up at (0,0), which made
	// every swipe classify as "direction toward the top-left corner".
	var p positionTracker
	down := func() time.Time { return time.Unix(1, 0) }
	move := func() time.Time { return time.Unix(2, 0) }
	up := func() time.Time { return time.Unix(3, 0) }

	f1 := p.frame([]rawEvent{
		{evKey, btnTouch, 1}, {evAbs, absX, 3000}, {evAbs, absY, 100}, {evSyn, synReport, 0},
	}, down)
	f2 := p.frame([]rawEvent{
		{evAbs, absX, 2900}, {evAbs, absY, 2500}, {evSyn, synReport, 0},
	}, move)
	f3 := p.frame([]rawEvent{
		{evKey, btnTouch, 0}, {evSyn, synReport, 0},
	}, up)

	if len(f1) != 1 || !f1[0].Down || f1[0].X != 3000 || f1[0].Y != 100 {
		t.Fatalf("down frame samples = %+v", f1)
	}
	if len(f2) != 1 || f2[0].Down || f2[0].Up || f2[0].X != 2900 || f2[0].Y != 2500 {
		t.Fatalf("move frame samples = %+v (movement must be emitted while pressed)", f2)
	}
	if len(f3) != 1 || !f3[0].Up {
		t.Fatalf("up frame samples = %+v", f3)
	}
	// The up position must be the last known position, NOT (0,0).
	if f3[0].X != 2900 || f3[0].Y != 2500 {
		t.Fatalf("up sample = (%d,%d), want held position (2900,2500)", f3[0].X, f3[0].Y)
	}

	// End-to-end through the recognizer: this trace is a DOWN swipe
	// (y 100 -> 2500). It must not be classified as a corner direction.
	rec := NewRecognizer(Options{})
	var gestures []Gesture
	for _, group := range [][]Sample{f1, f2, f3} {
		for _, s := range group {
			gestures = append(gestures, rec.Feed(s)...)
		}
	}
	if len(gestures) != 1 || gestures[0].Kind != KindSwipe || gestures[0].Dir != DirDown {
		t.Fatalf("gestures = %+v, want one down-swipe", gestures)
	}
}

func TestScaleAxis(t *testing.T) {
	cases := []struct {
		raw, min, max, size, want int
		why                       string
	}{
		{0, 0, 4096, 1920, 0, "left edge"},
		{4096, 0, 4096, 1920, 1919, "right edge"},
		{2048, 0, 4096, 1920, 959, "centre (truncating)"},
		{5000, 0, 4096, 1920, 1919, "clamped high"},
		{-10, 0, 4096, 1920, 0, "clamped low"},
		{100, 0, 0, 1920, 100, "no axis info: passthrough"},
		{1920, 0, 1919, 1920, 1919, "pixel-reporting device: clamp"},
		{100, 10, 110, 500, 449, "offset range"},
	}
	for _, c := range cases {
		if got := scaleAxis(c.raw, c.min, c.max, c.size); got != c.want {
			t.Errorf("scaleAxis(%d,%d,%d,%d) = %d, want %d (%s)", c.raw, c.min, c.max, c.size, got, c.want, c.why)
		}
	}
}

func TestFindTouchscreen(t *testing.T) {
	root := t.TempDir()
	sys := filepath.Join(root, "sys", "class", "input")
	dev := filepath.Join(root, "dev", "input")
	for _, d := range []string{
		filepath.Join(sys, "event0", "device"),
		filepath.Join(sys, "event1", "device"),
		filepath.Join(sys, "event2", "device"),
		dev,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// event0: keyboard (must not match); event1: Goodix touchscreen;
	// event2: mouse (must not match).
	os.WriteFile(filepath.Join(sys, "event0", "device", "name"), []byte("Pi Keyboard\n"), 0o644)
	os.WriteFile(filepath.Join(sys, "event1", "device", "name"), []byte("Goodix Technology GT911\n"), 0o644)
	os.WriteFile(filepath.Join(sys, "event2", "device", "name"), []byte("USB Mouse\n"), 0o644)
	// Only the touchscreen node exists in /dev.
	node1 := filepath.Join(dev, "event1")
	os.WriteFile(node1, nil, 0o644)

	got, err := FindTouchscreen(filepath.Join(root, "sys"), filepath.Join(root, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	if got != node1 {
		t.Errorf("found %q, want %q", got, node1)
	}
}

func TestFindTouchscreenUEventFallback(t *testing.T) {
	root := t.TempDir()
	sys := filepath.Join(root, "sys", "class", "input")
	dev := filepath.Join(root, "dev", "input")
	for _, d := range []string{
		filepath.Join(sys, "event3", "device"),
		dev,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// No "name" file; NAME= line in uevent only.
	os.WriteFile(filepath.Join(sys, "event3", "device", "uevent"),
		[]byte("DRIVER=i2c_hid\nNAME=I2C HID 0000:001B:Capacitive Touchscreen\n"), 0o644)
	node := filepath.Join(dev, "event3")
	os.WriteFile(node, nil, 0o644)

	got, err := FindTouchscreen(filepath.Join(root, "sys"), filepath.Join(root, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	if got != node {
		t.Errorf("found %q, want %q", got, node)
	}
}

func TestFindTouchscreenNone(t *testing.T) {
	root := t.TempDir()
	sys := filepath.Join(root, "sys", "class", "input")
	if err := os.MkdirAll(filepath.Join(sys, "event0", "device"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sys, "event0", "device", "name"), []byte("Keyboard\n"), 0o644)
	if _, err := FindTouchscreen(filepath.Join(root, "sys"), filepath.Join(root, "dev")); err == nil {
		t.Error("expected error when no touchscreen present")
	}
}
