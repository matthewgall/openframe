package ui

import (
	"time"

	"github.com/matthewgall/openframe/internal/frame/canvas"
)

// Settings is the set of user-adjustable frame settings (spec §10
// "Settings mode"). The settings screen edits a STAGED copy; nothing
// is applied until the user HOLDS the apply row (protection against
// accidental activation).
type Settings struct {
	Brightness        float64
	SlideshowInterval time.Duration
	Transition        string // none | crossfade | slide
	Orientation       int    // 0 | 90 | 180 | 270
	SleepOn           int    // minutes from midnight
	SleepOff          int
	ShowCaption       bool
	ShowClock         bool
}

// intervalChoices are the tappable slideshow interval presets.
var intervalChoices = []time.Duration{30 * time.Second, 45 * time.Second, 60 * time.Second, 2 * time.Minute, 5 * time.Minute}

// sleepOnChoices / sleepOffChoices are the tappable presets.
var (
	sleepOnChoices  = []int{6 * 60, 6*60 + 30, 7 * 60, 7*60 + 30, 8 * 60}
	sleepOffChoices = []int{21 * 60, 21*60 + 30, 22 * 60, 22*60 + 30, 23 * 60, 23*60 + 30, 24 * 60}
)

// SettingsFromConfig builds staged settings from the current config.
func SettingsFromConfig(c *Config) Settings {
	return Settings{
		Brightness:        c.Brightness,
		SlideshowInterval: c.Interval,
		Transition:        c.Transition,
		Orientation:       c.Orientation,
		SleepOn:           c.SleepOn,
		SleepOff:          c.SleepOff,
		ShowCaption:       c.ShowCaption,
		ShowClock:         c.ShowClock,
	}
}

// effectiveSettings rebuilds staged values from the live config (used
// when opening the settings screen, so external changes — e.g. a new
// config — are visible).
func (e *Engine) effectiveSettings() Settings {
	return SettingsFromConfig(e.cfg)
}

// applySettings runs the hold-protected apply: persist via the
// callback, then update the engine's live config so the change is
// immediate (the frame wiring persists to frame_settings + driver).
func (e *Engine) applySettings() {
	if e.cfg.ApplySettings == nil {
		e.log.Warn("settings apply unavailable")
		return
	}
	if err := e.cfg.ApplySettings(e.staged); err != nil {
		e.log.Error("settings apply failed", "error", err)
		return
	}
	e.applyLive(e.staged)
	e.log.Info("settings applied",
		"brightness", e.staged.Brightness,
		"interval", e.staged.SlideshowInterval.String(),
		"transition", e.staged.Transition,
		"orientation", e.staged.Orientation,
		"sleep", minutesToHM(e.staged.SleepOn)+"-"+minutesToHM(e.staged.SleepOff))
	e.render()
}

// applyLive reflects staged settings into the engine config (the
// callback handles persistence + driver effects).
func (e *Engine) applyLive(s Settings) {
	e.cfg.Interval = s.SlideshowInterval
	e.cfg.Transition = s.Transition
	e.cfg.ShowCaption = s.ShowCaption
	e.cfg.ShowClock = s.ShowClock
}

// doSetting handles a tap on a settings row (stage-only: nothing is
// persisted until hold-to-apply).
func (e *Engine) doSetting(index int) {
	switch index {
	case 0: // Brightness: tap left half -10%, right half +10%.
		e.staged.Brightness += 0.1
		if e.staged.Brightness < 0.1 {
			e.staged.Brightness = 0.1
		}
		if e.staged.Brightness > 1.0 {
			e.staged.Brightness = 1.0
		}
	case 1: // Slideshow interval: cycle presets.
		i := indexIn(intervalChoices, e.staged.SlideshowInterval)
		e.staged.SlideshowInterval = intervalChoices[(i+1)%len(intervalChoices)]
	case 2: // Transition: cycle.
		switch e.staged.Transition {
		case "none":
			e.staged.Transition = "crossfade"
		case "crossfade":
			e.staged.Transition = "slide"
		default:
			e.staged.Transition = "none"
		}
	case 3: // Orientation: cycle.
		switch e.staged.Orientation {
		case 0:
			e.staged.Orientation = 90
		case 90:
			e.staged.Orientation = 180
		case 180:
			e.staged.Orientation = 270
		default:
			e.staged.Orientation = 0
		}
	case 4: // Sleep on: cycle presets.
		i := indexInInt(sleepOnChoices, e.staged.SleepOn)
		e.staged.SleepOn = sleepOnChoices[(i+1)%len(sleepOnChoices)]
	case 5: // Sleep off: cycle presets.
		i := indexInInt(sleepOffChoices, e.staged.SleepOff)
		e.staged.SleepOff = sleepOffChoices[(i+1)%len(sleepOffChoices)]
	case 6: // Captions.
		e.staged.ShowCaption = !e.staged.ShowCaption
	case 7: // Clock.
		e.staged.ShowClock = !e.staged.ShowClock
	}
	e.render()
}

func indexIn(durations []time.Duration, d time.Duration) int {
	for i, v := range durations {
		if v == d {
			return i
		}
	}
	return 1 // default to 45s position
}

func indexInInt(vals []int, v int) int {
	for i, x := range vals {
		if x == v {
			return i
		}
	}
	return 2
}

// fitModeFromString is used when applying a staged fit (not user-tunable
// in v1; kept for completeness of the settings surface).
func fitModeFromString(s string) canvas.FitMode {
	switch s {
	case "cover":
		return canvas.FitCover
	case "crop":
		return canvas.FitCrop
	default:
		return canvas.FitContain
	}
}
