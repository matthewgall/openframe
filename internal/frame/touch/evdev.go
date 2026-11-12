// Package touch provides the frame's capacitive-touchscreen input
// pipeline (spec §10 "Touch input"): a pure evdev event parser, a
// pure gesture recognizer, and a thin device reader.
//
// The parser and recognizer are pure and fully unit-tested; only the
// device reader touches /dev/input, which is why it can be validated
// on the Pi while everything above it is validated in CI.
package touch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"
)

// evdev event types and codes (linux/input-event-codes.h).
const (
	evSyn     = 0x00
	evKey     = 0x01
	evAbs     = 0x03
	synReport = 0x00
	btnTouch  = 0x14b
	absX      = 0x00
	absY      = 0x01
)

// eventSize is sizeof(struct input_event) on 64-bit Linux:
// timeval(16) + type(2) + code(2) + value(4) = 24.
const eventSize = 24

// Sample is one finger position, emitted on each EV_SYN while the
// finger is down. Down/Up mark the contact edges.
type Sample struct {
	X, Y int
	T    time.Time
	Down bool
	Up   bool
}

// rawEvent is a decoded input_event.
type rawEvent struct {
	Type, Code int
	Value      int
}

// ParseEvents decodes complete 24-byte input_event records from buf,
// in order. A trailing partial record is ignored (the device reader
// re-supplies the buffer boundary is not an error). The timestamp is
// dropped here — the caller stamps samples with its own clock, which
// keeps parsing pure and testable.
func ParseEvents(buf []byte) []rawEvent {
	n := len(buf) / eventSize
	out := make([]rawEvent, 0, n)
	for i := 0; i < n; i++ {
		b := buf[i*eventSize : (i+1)*eventSize]
		// Skip the 16-byte timeval; type/code/value are at 16/18/20.
		typ := int(binary.LittleEndian.Uint16(b[16:]))
		code := int(binary.LittleEndian.Uint16(b[18:]))
		val := int(int32(binary.LittleEndian.Uint32(b[20:])))
		out = append(out, rawEvent{Type: typ, Code: code, Value: val})
	}
	return out
}

// errShortRead is returned when a partial event was read; the caller
// should simply retry.
var errShortRead = errors.New("touch: short read")

// Device is an open evdev node.
type Device struct {
	path string
	f    *os.File
}

// Open opens an evdev node for reading.
func Open(path string) (*Device, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("touch: open %s: %w", path, err)
	}
	return &Device{path: path, f: f}, nil
}

// Path returns the device node path.
func (d *Device) Path() string { return d.path }

// Close releases the device.
func (d *Device) Close() error { return d.f.Close() }

// readEvents blocks until at least one complete SYN_REPORT is seen and
// returns the decoded raw events for that frame (plus any preceding
// partial frame data). It reassembles across read() boundaries.
func (d *Device) readFrame() ([]rawEvent, error) {
	pending := []byte{}
	for {
		buf := make([]byte, 256)
		n, err := d.f.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
		}
		if err != nil {
			if n == 0 {
				return nil, err
			}
			// EINTR etc.: keep what we have and retry.
		}
		// Find the first SYN_REPORT in pending.
		evs := ParseEvents(pending)
		for i, e := range evs {
			if e.Type == evSyn && e.Code == synReport {
				return evs[:i+1], nil
			}
		}
		// No complete report yet. Keep the last partial record so it
		// isn't lost, and block for more.
		complete := (len(pending) / eventSize) * eventSize
		partial := len(pending) - complete
		if partial > 0 {
			tail := make([]byte, partial)
			copy(tail, pending[len(pending)-partial:])
			pending = tail
		} else {
			pending = nil
		}
	}
}

// SampleFrames converts a stream of raw events into finger Samples,
// stamping each SYN-emitted sample with now(). Pure and testable.
func SampleFrames(evs []rawEvent, now func() time.Time) []Sample {
	var out []Sample
	x, y := 0, 0
	pressed := false
	edgeDown, edgeUp := false, false
	for _, e := range evs {
		switch {
		case e.Type == evAbs && e.Code == absX:
			x = e.Value
		case e.Type == evAbs && e.Code == absY:
			y = e.Value
		case e.Type == evKey && e.Code == btnTouch:
			if e.Value == 1 {
				pressed = true
				edgeDown = true
				edgeUp = false
			} else {
				pressed = false
				edgeUp = true
				edgeDown = false
			}
		case e.Type == evSyn && e.Code == synReport:
			if pressed || edgeUp {
				s := Sample{X: x, Y: y, T: now(), Up: edgeUp}
				if edgeDown {
					s.Down = true
				}
				out = append(out, s)
			}
			edgeDown, edgeUp = false, false
		}
	}
	return out
}
