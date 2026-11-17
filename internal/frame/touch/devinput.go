package touch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DevInput is a development seam that injects finger Samples into the
// real gesture recognizer — the exact same Sample channel an evdev
// device produces. It exists so the UI (menus, settings, gestures)
// can be exercised on headless hardware (CI, dev boxes, a server with
// no touchscreen) using the headless driver's snapshots as the
// "screen".
//
// It reads a small line protocol from a FIFO (named pipe):
//
//	tap X Y          short press + release        → Tap
//	dtap X Y         two quick taps               → DoubleTap
//	hold X Y [ms]    press, wait (default 900),   → Hold
//	                 release
//	swipe L|U|R|D    swipe from centre (640,360); → Swipe
//	                 "swipe L X Y" picks the start
//	down X Y         raw: finger down
//	move X Y         raw: finger move
//	up [X Y]         raw: finger up (pos = last move if omitted)
//	wait [ms]        script delay (default 100)
//	# comment
//
// "swipe" and friends are NOT pre-classified gestures: they expand to
// down/move/up sample sequences, so the real classifier runs end to
// end. Unknown lines are logged and ignored.
//
// This is a development aid, not a security surface: the FIFO must be
// owned by the frame's user, and on a production frame no one has
// write access to it (and the config key is unset anyway).
type DevInput struct {
	lines chan string
	log   *slog.Logger

	mu    sync.Mutex
	lastX int
	lastY int
}

// NewDevInput builds a DevInput over an explicit line channel (tests).
// The channel may be closed; the stream then waits for ctx.
func NewDevInput(lines chan string, log *slog.Logger) *DevInput {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &DevInput{lines: lines, log: log}
}

// DevInputFromFile serves the protocol from a FIFO, creating it (and
// its parent directories) if missing. Opening a FIFO blocks until a
// writer attaches, so the read loop runs in a goroutine and simply
// re-opens after each writer session — the input survives
// `echo "tap 1 2" > fifo` one-liners and their close.
func DevInputFromFile(path string, log *slog.Logger) (*DevInput, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := createFifo(path); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if st.Mode()&os.ModeNamedPipe == 0 {
		return nil, fmt.Errorf("dev input: %s is not a fifo", path)
	}
	lines := make(chan string, 32)
	go func() {
		for {
			f, err := os.Open(path)
			if err != nil {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
			for sc.Scan() {
				lines <- sc.Text()
			}
			f.Close() // last writer went away; wait for the next
		}
	}()
	return NewDevInput(lines, log), nil
}

// Stream emits one Sample per finger position, like Device.Stream.
// The channel closes when ctx is cancelled.
func (d *DevInput) Stream(ctx context.Context) <-chan Sample {
	ch := make(chan Sample, 64)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case line, ok := <-d.lines:
				if !ok { // test channel closed: wait for ctx
					<-ctx.Done()
					return
				}
				d.exec(line, time.Now, func(s Sample) {
					select {
					case ch <- s:
					case <-ctx.Done():
					}
				})
			}
		}
	}()
	return ch
}

// exec runs one protocol line, emitting samples via emit. now is the
// clock (injected in tests).
func (d *DevInput) exec(line string, now func() time.Time, emit func(Sample)) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	fields := strings.Fields(line)
	kind := strings.ToLower(fields[0])
	num := func(i int) int {
		v, _ := strconv.Atoi(fields[i])
		return v
	}

	press := func(x, y int) {
		d.setLast(x, y)
		emit(Sample{X: x, Y: y, T: now(), Down: true})
	}
	release := func(x, y int) {
		d.setLast(x, y)
		emit(Sample{X: x, Y: y, T: now(), Up: true})
	}
	tap := func(x, y int) {
		press(x, y)
		time.Sleep(120 * time.Millisecond) // < TapMaxDuration (300ms)
		release(x, y)
	}

	switch kind {
	case "tap":
		if len(fields) < 3 {
			d.log.Warn("dev input: usage: tap X Y")
			return
		}
		tap(num(1), num(2))
	case "dtap":
		if len(fields) < 3 {
			d.log.Warn("dev input: usage: dtap X Y")
			return
		}
		tap(num(1), num(2))
		time.Sleep(150 * time.Millisecond) // < DoubleTapGap (320ms)
		tap(num(1), num(2))
	case "hold":
		if len(fields) < 3 {
			d.log.Warn("dev input: usage: hold X Y [ms]")
			return
		}
		x, y := num(1), num(2)
		ms := 900
		if len(fields) >= 4 {
			ms = num(3)
		}
		if ms < 600 {
			ms = 600 // never under HoldMinDuration (550ms) by accident
		}
		press(x, y)
		time.Sleep(time.Duration(ms) * time.Millisecond)
		release(x, y)
	case "swipe":
		if len(fields) < 2 {
			d.log.Warn("dev input: usage: swipe L|U|R|D [X Y]")
			return
		}
		var sx, sy, ex, ey int
		sx, sy = 640, 360 // panel centre for 1280x800-class panels
		if len(fields) >= 4 {
			sx, sy = num(2), num(3)
		}
		switch strings.ToLower(fields[1]) {
		case "left", "l":
			ex, ey = sx-560, sy
		case "right", "r":
			ex, ey = sx+560, sy
		case "up", "u":
			ex, ey = sx, sy-280
		case "down", "d":
			ex, ey = sx, sy+280
		default:
			d.log.Warn("dev input: usage: swipe L|U|R|D [X Y]")
			return
		}
		d.swipeTo(sx, sy, ex, ey, now, emit)
	case "down":
		if len(fields) < 3 {
			d.log.Warn("dev input: usage: down X Y")
			return
		}
		press(num(1), num(2))
	case "move":
		if len(fields) < 3 {
			d.log.Warn("dev input: usage: move X Y")
			return
		}
		d.setLast(num(1), num(2))
		emit(Sample{X: num(1), Y: num(2), T: now()})
	case "up":
		x, y := d.lastPos()
		if len(fields) >= 3 {
			x, y = num(1), num(2)
		}
		release(x, y)
	case "wait":
		ms := 100
		if len(fields) >= 2 {
			ms = num(1)
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
	default:
		d.log.Warn("dev input: unknown command (tap/dtap/hold/swipe/down/move/up/wait)", "line", line)
	}
}

// swipeTo emits down, 5 interpolated moves and up — a gesture the
// classifier recognises as a swipe (> 60px, < 550ms).
func (d *DevInput) swipeTo(sx, sy, ex, ey int, now func() time.Time, emit func(Sample)) {
	const steps = 5
	d.setLast(sx, sy)
	emit(Sample{X: sx, Y: sy, T: now(), Down: true})
	for i := 1; i <= steps; i++ {
		x := sx + (ex-sx)*i/steps
		y := sy + (ey-sy)*i/steps
		time.Sleep(30 * time.Millisecond)
		d.setLast(x, y)
		emit(Sample{X: x, Y: y, T: now()})
	}
	time.Sleep(30 * time.Millisecond)
	d.setLast(ex, ey)
	emit(Sample{X: ex, Y: ey, T: now(), Up: true})
}

func (d *DevInput) setLast(x, y int) {
	d.mu.Lock()
	d.lastX, d.lastY = x, y
	d.mu.Unlock()
}

func (d *DevInput) lastPos() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastX, d.lastY
}
