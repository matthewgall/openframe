package ui

import (
	"image/color"
	"time"
)

// --- arrival debuts (living-presentation F2 / R7-R11) ---------------
//
// Activation hands the engine a short list of photo hashes new to the
// previously-active set (sync layer's job; capped, upload-ordered).
// The UI queues up to maxDebuts of them; the next slideshow advance
// jumps to the first one still in the rotation and "arms" a ribbon —
// a top-left "from Sarah" pill that ticks down ~8 s of visible,
// UNHELD time (controls open or asleep freeze the countdown; the
// moment must not die behind the overlay or during the night). A
// ribbon belongs to exactly one photo: advancing away from it spends
// the debut (it is not deferred to whoever is shown next).

const (
	maxDebuts    = 3
	ribbonShow   = 8 * time.Second
	ribbonFade   = 2 * time.Second // last stretch: scrim fades, glyphs hold
	ribbonSecond = time.Second     // Tick granularity the countdown rides
)

type ribbon struct {
	hash string
	text string
	left time.Duration
}

// QueueArrivals records arrived hashes for debut at coming advances.
// Deduped, capped at maxDebuts; unknown-or-later-deleted hashes are
// dropped when they surface (advance / debutIndex), never here — an
// arrival may be queued before the reload that contains it lands in
// any view other than "all".
func (e *Engine) QueueArrivals(hashes []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, h := range hashes {
		if h == "" || len(e.debuts) >= maxDebuts {
			continue
		}
		dup := false
		for _, q := range e.debuts {
			if q == h {
				dup = true
				break
			}
		}
		if !dup {
			e.debuts = append(e.debuts, h)
		}
	}
	// A queued photo already on screen debuts without waiting.
	e.syncDebutLocked()
	e.render()
}

// debutIndexLocked returns the list index of the first queued debut
// still present in the current rotation, dropping stale queue heads
// (photos deleted between activation and debut) as it scans. -1 when
// the queue holds nothing displayable.
func (e *Engine) debutIndexLocked() int {
	for len(e.debuts) > 0 {
		h := e.debuts[0]
		for i := range e.list {
			if e.list[i].Hash == h {
				return i
			}
		}
		e.debuts = e.debuts[1:] // vanished before its moment
	}
	return -1
}

// syncDebutLocked arms or cancels the ribbon against the currently
// shown photo. Call with e.mu held, after e.idx/curImage updates.
func (e *Engine) syncDebutLocked() {
	if len(e.list) == 0 || e.idx < 0 || e.idx >= len(e.list) {
		e.ribbon = nil
		return
	}
	cur := e.list[e.idx]
	// Away from the armed photo: the debut is spent.
	if e.ribbon != nil && e.ribbon.hash != cur.Hash {
		e.ribbon = nil
	}
	// On-screen queued photo: consume and arm.
	for i, h := range e.debuts {
		if h != cur.Hash {
			continue
		}
		e.debuts = append(e.debuts[:i], e.debuts[i+1:]...)
		text := "new photo"
		if cur.Uploader != "" {
			text = "from " + cur.Uploader
		}
		e.ribbon = &ribbon{hash: cur.Hash, text: text, left: ribbonShow}
		return
	}
}

// ribbonHeld reports whether the fade clock is paused (the moment is
// not being watched: controls are up or the frame is asleep).
func (e *Engine) ribbonHeld() bool { return e.controls || e.asleep }

// drawRibbon paints the armed ribbon (top-left band: clear of the
// top-right clock pill, the bottom-left favourite badge, and the
// caption bar — R8). Z-order is photo -> ribbon -> controls: called
// after the caption/star, before the controls overlay. The scrim
// fades across ribbonFade; glyph alpha stays at full (>= 90 floor for
// the older-viewer persona — contrast is what readability needs).
func (e *Engine) drawRibbon() {
	r := e.ribbon
	if r == nil || len(e.list) == 0 || e.idx < 0 || e.idx >= len(e.list) ||
		e.list[e.idx].Hash != r.hash {
		return
	}
	f := e.cv.Font(fontBody)
	text := r.text
	// Long display names truncate to the band width; the pill sizes
	// to what is actually drawn.
	maxRunes := (e.w - 4*margin - f.Width("from ")) / f.CellW()
	if maxRunes < 4 {
		maxRunes = 4
	}
	text = truncate(text, maxRunes)
	tw := f.Width(text)
	pw, ph := tw+32, f.CellH()+20
	x, y := margin, margin
	alpha := uint8(140)
	if r.left < ribbonFade {
		alpha = uint8(int64(140) * int64(r.left) / int64(ribbonFade))
	}
	e.cv.FillRect(x, y, pw, ph, color.NRGBA{R: 0, G: 0, B: 0, A: alpha})
	f.DrawText(e.cv.Image(), x+16, y+(ph-f.CellH())/2, text, colText)
}
