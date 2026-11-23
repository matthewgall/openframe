package ui

import (
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/frame/touch"
)

// The settings list is scrollable; users need to SEE that. Chrome:
// proportional thumb + page arrows at the right edge, with the
// arrow at a boundary dimmed and not hit-registered.
func TestSettingsScrollChrome(t *testing.T) {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	env.nowAt(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	// Gallery -> Settings.
	env.swipe(touch.DirDown)
	var settingsBtn *hitButton
	for i := range e.buttons {
		if e.buttons[i].kind == btnSettings {
			settingsBtn = &e.buttons[i]
		}
	}
	if settingsBtn == nil {
		t.Fatal("settings row not found")
	}
	env.tap((settingsBtn.rect[0]+settingsBtn.rect[2])/2, (settingsBtn.rect[1]+settingsBtn.rect[3])/2)
	if e.Screen() != ScreenSettings {
		t.Fatalf("screen = %v, want settings", e.Screen())
	}

	if e.settingsMaxTop <= 0 {
		t.Skip("viewport fits all settings rows at this size; chrome not shown")
	}

	// At the top: down arrow present, up arrow absent.
	up, down := scrollBtns(e)
	if up != nil {
		t.Error("up arrow registered at top of list")
	}
	if down == nil {
		t.Fatal("down arrow not registered")
	}

	env.tap((down.rect[0]+down.rect[2])/2, (down.rect[1]+down.rect[3])/2)
	if e.settingsTop < 1 {
		t.Fatalf("down arrow tap: top = %d, want >= 1", e.settingsTop)
	}
	topBefore := e.settingsTop

	// Now up must exist.
	up, _ = scrollBtns(e)
	if up == nil {
		t.Fatal("up arrow not registered after scrolling")
	}
	env.tap((up.rect[0]+up.rect[2])/2, (up.rect[1]+up.rect[3])/2)
	if e.settingsTop >= topBefore {
		t.Fatalf("up arrow tap: top = %d, want < %d", e.settingsTop, topBefore)
	}

	// Page all the way down: top stops at maxTop, down arrow dims.
	for i := 0; i < 12; i++ {
		if _, d := scrollBtns(e); d == nil {
			break
		}
		_, d := scrollBtns(e)
		env.tap((d.rect[0]+d.rect[2])/2, (d.rect[1]+d.rect[3])/2)
	}
	if e.settingsTop != e.settingsMaxTop {
		t.Errorf("top = %d, want %d at bottom", e.settingsTop, e.settingsMaxTop)
	}
	if _, d := scrollBtns(e); d != nil {
		t.Error("down arrow still registered at bottom")
	}
}

func scrollBtns(e *Engine) (up, down *hitButton) {
	for i := range e.buttons {
		switch e.buttons[i].kind {
		case btnScrollUp:
			up = &e.buttons[i]
		case btnScrollDown:
			down = &e.buttons[i]
		}
	}
	return
}
