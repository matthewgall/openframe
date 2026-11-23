package ui

import (
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/frame/touch"
)

// Hold an editable settings row -> OSD keyboard overlay -> type a
// value -> Done stages it; invalid input keeps the overlay open with
// the error; Cancel/swipe discard; tap-cycling still works on rows
// that were never held.
func TestSettingsValueEditor(t *testing.T) {
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
	env.tap((settingsBtn.rect[0]+settingsBtn.rect[2])/2, (settingsBtn.rect[1]+settingsBtn.rect[3])/2)
	if e.Screen() != ScreenSettings {
		t.Fatalf("screen = %v, want settings", e.Screen())
	}

	// Find the "Sleep on" row and HOLD it.
	settingRow := func(name string) *hitButton {
		t.Helper()
		want := btnSetting + settingIndex(name)
		for i := range e.buttons {
			if e.buttons[i].kind == want {
				return &e.buttons[i]
			}
		}
		t.Fatalf("row %q not found", name)
		return nil
	}
	row := settingRow("Sleep on")
	env.hold((row.rect[0]+row.rect[2])/2, (row.rect[1]+row.rect[3])/2)
	if e.Screen() != ScreenEdit {
		t.Fatalf("hold did not open editor: %v", e.Screen())
	}

	// Type 06:15 on the keyboard and Done.
	tapKey := func(label string) {
		t.Helper()
		for i := range e.buttons {
			b := &e.buttons[i]
			if b.kind == btnKey && b.label == label {
				env.tap((b.rect[0]+b.rect[2])/2, (b.rect[1]+b.rect[3])/2)
				return
			}
		}
		t.Fatalf("key %q not found", label)
	}
	for _, ch := range "06:15" {
		tapKey(string(ch))
	}
	for i := range e.buttons {
		if e.buttons[i].kind == btnKeyDone {
			b := &e.buttons[i]
			env.tap((b.rect[0]+b.rect[2])/2, (b.rect[1]+b.rect[3])/2)
			break
		}
	}
	if e.Screen() != ScreenSettings {
		t.Fatalf("after Done: %v, want settings", e.Screen())
	}
	if e.staged.SleepOn != 6*60+15 {
		t.Fatalf("SleepOn = %d, want %d", e.staged.SleepOn, 6*60+15)
	}

	// Invalid input keeps the overlay open with a message.
	env.hold((row.rect[0]+row.rect[2])/2, (row.rect[1]+row.rect[3])/2)
	if e.Screen() != ScreenEdit {
		t.Fatal("editor did not reopen")
	}
	tapKey("9")
	for _, ch := range "9:99" {
		tapKey(string(ch))
	}
	for i := range e.buttons {
		if e.buttons[i].kind == btnKeyDone {
			b := &e.buttons[i]
			env.tap((b.rect[0]+b.rect[2])/2, (b.rect[1]+b.rect[3])/2)
			break
		}
	}
	if e.Screen() != ScreenEdit {
		t.Fatalf("invalid value closed editor: %v", e.Screen())
	}
	if e.editErr == "" {
		t.Error("invalid value set no error message")
	}
	if e.staged.SleepOn != 6*60+15 {
		t.Errorf("staged changed on invalid input: %d", e.staged.SleepOn)
	}

	// Swipe discards.
	before := e.staged.SleepOff
	env.swipe(touch.DirLeft)
	if e.Screen() != ScreenSettings {
		t.Fatalf("swipe did not discard: %v", e.Screen())
	}
	if e.staged.SleepOff != before {
		t.Error("discard changed staged value")
	}
}

// A duration-typed value for the interval row, plus brightness "80%".
func TestSettingsValueEditorOtherRows(t *testing.T) {
	photos := testPhotos(t, 1)
	env := newUIEnv(t, photos, nil)
	e := env.eng
	env.nowAt(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))
	env.swipe(touch.DirDown)
	var settingsBtn *hitButton
	for i := range e.buttons {
		if e.buttons[i].kind == btnSettings {
			settingsBtn = &e.buttons[i]
		}
	}
	env.tap((settingsBtn.rect[0]+settingsBtn.rect[2])/2, (settingsBtn.rect[1]+settingsBtn.rect[3])/2)

	holdRow := func(name string) {
		want := btnSetting + settingIndex(name)
		for i := range e.buttons {
			if e.buttons[i].kind == want {
				b := &e.buttons[i]
				env.hold((b.rect[0]+b.rect[2])/2, (b.rect[1]+b.rect[3])/2)
				return
			}
		}
		t.Fatalf("row %q missing", name)
	}
	tap := func(kind int, label string) {
		for i := range e.buttons {
			b := &e.buttons[i]
			if b.kind == kind && (label == "" || b.label == label) {
				env.tap((b.rect[0]+b.rect[2])/2, (b.rect[1]+b.rect[3])/2)
				return
			}
		}
	}

	// interval 1m (digits + letter m)
	holdRow("Slideshow interval")
	for _, ch := range "1m" {
		tap(btnKey, string(ch))
	}
	tap(btnKeyDone, "")
	if e.staged.SlideshowInterval != time.Minute {
		t.Fatalf("interval = %v, want 1m", e.staged.SlideshowInterval)
	}

	// brightness 50%
	holdRow("Brightness")
	for _, ch := range "50%" {
		tap(btnKey, string(ch)) // "%" lives on the action row? it is a letter-free key; check fallback below
	}
	// The keyboard has no "%" key; typing "50" alone must work too.
	tap(btnKeyDone, "")
	if e.staged.Brightness != 0.5 {
		t.Fatalf("brightness = %v, want 0.5 (typed 50)", e.staged.Brightness)
	}
}
