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
