package ui

import (
	"testing"
	"time"
)

// The link glyph lives inside the clock pill at the panel's top
// right. Photos in this harness are red (G=0), the pill background
// is black (G=0), and the time text is to the RIGHT of the glyph
// block - so any green-channel light in the glyph box can only come
// from the glyph itself.
func TestClockLinkGlyph(t *testing.T) {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	env.nowAt(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	e.Reload(t.Context())

	glyphMaxG := func() int {
		// Recompute the glyph box from the same constants drawClock
		// uses (font metrics are deterministic in the harness).
		f := e.cv.Font(fontBody)
		tw := f.Width("12:00")
		x0 := e.w - 40 - tw - 2*margin
		maxG := 0
		for x := x0 + 2; x < x0+36; x++ {
			for y := 4; y < 46; y++ {
				if g := int(env.px(x, y).G); g > maxG {
					maxG = g
				}
			}
		}
		return maxG
	}

	e.SetLink(LinkWiFi, true)
	if got := glyphMaxG(); got < 150 {
		t.Errorf("wifi glyph brightness G=%d, want bright mark (>=150)", got)
	}

	e.SetLink(LinkWired, false)
	if got := glyphMaxG(); got < 120 {
		t.Errorf("wired-unreachable glyph G=%d, want dim-gray mark (>=120)", got)
	}

	e.SetLink(LinkNone, false)
	if got := glyphMaxG(); got > 110 {
		t.Errorf("offline glyph G=%d, want nearly invisible (<=110)", got)
	}
}
