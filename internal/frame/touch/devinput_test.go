package touch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// runProtocol feeds lines through a DevInput stream and the real
// recognizer, returning the gestures it produced.
func runProtocol(t *testing.T, lines ...string) []Gesture {
	t.Helper()
	ch := make(chan string, len(lines))
	for _, l := range lines {
		ch <- l
	}
	di := NewDevInput(ch, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rec := NewRecognizer(Options{})
	var out []Gesture
	stream := di.Stream(ctx)
	// The protocol executor sleeps real time between samples; drain
	// until quiet, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	idle := 0
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the dev input to settle")
		}
		select {
		case s, ok := <-stream:
			if !ok {
				return out
			}
			idle = 0
			out = append(out, rec.Feed(s)...)
		case <-time.After(400 * time.Millisecond):
			idle++
			if idle >= 2 {
				return out
			}
		}
	}
}

func assertSingle(t *testing.T, gs []Gesture, kind Kind, pos [2]int, dir Dir) {
	t.Helper()
	if len(gs) != 1 {
		t.Fatalf("gestures: %v, want exactly 1 (%s)", gs, kind)
	}
	if gs[0].Kind != kind {
		t.Fatalf("kind = %s, want %s (all: %v)", gs[0].Kind, kind, gs)
	}
	if pos != [2]int{} && gs[0].Pos != pos {
		t.Fatalf("pos = %v, want %v", gs[0].Pos, pos)
	}
	if dir != DirUp && dir != DirDown && dir != DirLeft && dir != DirRight && gs[0].Dir != dir {
		t.Fatalf("dir = %s, want %s", gs[0].Dir, dir)
	}
}

func TestDevInputTap(t *testing.T) {
	gs := runProtocol(t, "# a comment", "", "tap 100 120")
	assertSingle(t, gs, KindTap, [2]int{100, 120}, Dir(0))
}

func TestDevInputDoubleTap(t *testing.T) {
	// A double-tap is a tap that turns out to be a double-tap: the
	// recognizer emits the first tap as soon as it completes, then the
	// double-tap. The UI sees both (controls reveal, then details).
	gs := runProtocol(t, "dtap 40 50")
	if len(gs) != 2 || gs[0].Kind != KindTap || gs[1].Kind != KindDoubleTap {
		t.Fatalf("gestures: %v, want [tap double-tap]", gs)
	}
}

func TestDevInputHold(t *testing.T) {
	gs := runProtocol(t, "hold 50 60 700")
	assertSingle(t, gs, KindHold, [2]int{50, 60}, Dir(0))
}

func TestDevInputSwipe(t *testing.T) {
	for _, tc := range []struct {
		line string
		dir  Dir
	}{
		{"swipe left", DirLeft},
		{"swipe right", DirRight},
		{"swipe up", DirUp},
		{"swipe down", DirDown},
		{"swipe l 300 300", DirLeft},
	} {
		t.Run(tc.line, func(t *testing.T) {
			gs := runProtocol(t, tc.line)
			if len(gs) != 1 || gs[0].Kind != KindSwipe || gs[0].Dir != tc.dir {
				t.Fatalf("%q: %v, want Swipe %s", tc.line, gs, tc.dir)
			}
		})
	}
}

func TestDevInputRawSamples(t *testing.T) {
	// down + a long move + up = swipe (the classifier does the work).
	gs := runProtocol(t, "down 100 300", "move 700 300", "up")
	assertSingle(t, gs, KindSwipe, [2]int{700, 300}, DirRight)
}

func TestDevInputUnknownLineIgnored(t *testing.T) {
	gs := runProtocol(t, "frobulate 1 2", "tap 10 20")
	assertSingle(t, gs, KindTap, [2]int{10, 20}, Dir(0))
}

// TestDevInputFromFile exercises the real FIFO: create, attach,
// one-liner write, and the re-open after the writer closes.
func TestDevInputFromFile(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "touch.fifo")

	di, err := DevInputFromFile(fifo, nil)
	if err != nil {
		t.Fatalf("DevInputFromFile: %v", err)
	}
	st, err := os.Lstat(fifo)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if st.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("fifo was not created as a named pipe")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := NewRecognizer(Options{})
	var out []Gesture
	stream := di.Stream(ctx)

	write := func(line string) {
		t.Helper()
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open fifo: %v", err)
		}
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
		f.Close()
	}
	write("tap 11 22")

	deadline := time.Now().Add(5 * time.Second)
	for len(out) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for tap through the fifo")
		}
		select {
		case s, ok := <-stream:
			if !ok {
				t.Fatal("stream closed early")
			}
			out = append(out, rec.Feed(s)...)
		case <-time.After(50 * time.Millisecond):
		}
	}
	assertSingle(t, out, KindTap, [2]int{11, 22}, Dir(0))

	// A second writer session must work (the reader re-opens).
	time.Sleep(300 * time.Millisecond)
	write("tap 33 44")
	deadline = time.Now().Add(5 * time.Second)
	for len(out) == 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for second tap through the fifo")
		}
		select {
		case s, ok := <-stream:
			if !ok {
				t.Fatal("stream closed early")
			}
			out = append(out, rec.Feed(s)...)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestDevInputRejectsRegularFile: a plain file is not a valid source.
func TestDevInputRejectsRegularFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "not-a-fifo")
	if err := os.WriteFile(p, []byte("tap 1 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DevInputFromFile(p, nil); err == nil {
		t.Fatal("expected an error for a regular file")
	}
}
