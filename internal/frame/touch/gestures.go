package touch

import (
	"math"
	"time"
)

// Kind is a recognised gesture.
type Kind int

const (
	KindTap Kind = iota
	KindDoubleTap
	KindSwipe
	KindHold
)

// String is for logs and tests.
func (k Kind) String() string {
	switch k {
	case KindTap:
		return "tap"
	case KindDoubleTap:
		return "double-tap"
	case KindSwipe:
		return "swipe"
	case KindHold:
		return "hold"
	}
	return "unknown"
}

// Dir is a swipe direction.
type Dir int

const (
	DirNone Dir = iota
	DirLeft
	DirRight
	DirUp
	DirDown
)

func (d Dir) String() string {
	switch d {
	case DirLeft:
		return "left"
	case DirRight:
		return "right"
	case DirUp:
		return "up"
	case DirDown:
		return "down"
	}
	return "none"
}

// Gesture is a completed interaction.
type Gesture struct {
	Kind Kind
	Dir  Dir
	// Pos is the finger-up location (or tap location).
	Pos [2]int
	T   time.Time
}

// Options tunes the recognizer. Zero values select the defaults,
// which are chosen for a 1280x800 panel with a single finger.
type Options struct {
	TapMaxDuration  time.Duration // default 300ms
	TapMaxMove      int           // default 24px
	SwipeMinMove    int           // default 60px
	HoldMinDuration time.Duration // default 550ms
	DoubleTapGap    time.Duration // default 320ms
}

func (o Options) withDefaults() Options {
	if o.TapMaxDuration == 0 {
		o.TapMaxDuration = 300 * time.Millisecond
	}
	if o.TapMaxMove == 0 {
		o.TapMaxMove = 24
	}
	if o.SwipeMinMove == 0 {
		o.SwipeMinMove = 60
	}
	if o.HoldMinDuration == 0 {
		o.HoldMinDuration = 550 * time.Millisecond
	}
	if o.DoubleTapGap == 0 {
		o.DoubleTapGap = 320 * time.Millisecond
	}
	return o
}

// Recognizer turns a stream of finger Samples into Gestures. It is
// stateful per stream (call Feed in order) and pure w.r.t. I/O, so it
// is trivially testable by feeding synthetic samples.
type Recognizer struct {
	o Options

	downT   time.Time
	downX   int
	downY   int
	lastX   int
	lastY   int
	lastT   time.Time
	lastUp  time.Time
	inTouch bool
}

// NewRecognizer builds a Recognizer with the given options (zero =
// defaults).
func NewRecognizer(o Options) *Recognizer {
	return &Recognizer{o: o.withDefaults()}
}

// Feed consumes one sample and returns the gestures it completes (0 or
// 1). A tap/double-tap/swipe completes on finger-up; a hold completes
// on finger-up after the minimum duration (a hold that is lifted early
// is a tap or swipe by the other thresholds).
func (r *Recognizer) Feed(s Sample) []Gesture {
	switch {
	case s.Down:
		r.inTouch = true
		r.downT, r.downX, r.downY = s.T, s.X, s.Y
		r.lastX, r.lastY, r.lastT = s.X, s.Y, s.T
		return nil
	case s.Up:
		if !r.inTouch {
			return nil
		}
		r.inTouch = false
		// classify owns lastUp: it must see the PREVIOUS up, not this
		// one, when deciding between tap and double-tap.
		return r.classify(s.X, s.Y, s.T)
	default: // movement
		if !r.inTouch {
			return nil
		}
		r.lastX, r.lastY, r.lastT = s.X, s.Y, s.T
		return nil
	}
}

func (r *Recognizer) classify(upX, upY int, upT time.Time) []Gesture {
	dx := upX - r.downX
	dy := upY - r.downY
	dist := int(math.Hypot(float64(dx), float64(dy)))
	dur := upT.Sub(r.downT)

	// Hold: stayed put for long enough.
	if dur >= r.o.HoldMinDuration && dist < r.o.TapMaxMove {
		return []Gesture{{Kind: KindHold, Pos: [2]int{upX, upY}, T: upT}}
	}
	// Swipe: moved far enough along a dominant axis.
	if dist >= r.o.SwipeMinMove {
		dir := DirNone
		switch {
		case dx < 0 && math.Abs(float64(dx)) >= math.Abs(float64(dy)):
			dir = DirLeft
		case dx > 0 && math.Abs(float64(dx)) >= math.Abs(float64(dy)):
			dir = DirRight
		case dy < 0:
			dir = DirUp
		case dy > 0:
			dir = DirDown
		}
		if dir != DirNone {
			return []Gesture{{Kind: KindSwipe, Dir: dir, Pos: [2]int{upX, upY}, T: upT}}
		}
	}
	// Tap / double-tap: short and still.
	if dur < r.o.TapMaxDuration && dist < r.o.TapMaxMove {
		if !r.lastUp.IsZero() && upT.Sub(r.lastUp) < r.o.DoubleTapGap {
			r.lastUp = time.Time{}
			return []Gesture{{Kind: KindDoubleTap, Pos: [2]int{upX, upY}, T: upT}}
		}
		r.lastUp = upT
		return []Gesture{{Kind: KindTap, Pos: [2]int{upX, upY}, T: upT}}
	}
	return nil
}
