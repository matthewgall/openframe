package frame

import (
	"context"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/config"
)

func TestLocalSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, dbh := newTestFrame(t)

	// Nothing saved: empty map, config untouched.
	s := LoadLocalSettings(ctx, dbh)
	if len(s) != 0 {
		t.Fatalf("fresh db has settings: %v", s)
	}

	want := map[string]string{
		settingBrightness:        "0.4",
		settingSlideshowInterval: "60s",
		settingTransition:        "slide",
		settingOrientation:       "90",
		settingSleepOn:           "06:30",
		settingSleepOff:          "23:00",
		settingShowCaption:       "false",
		settingShowClock:         "true",
	}
	for k, v := range want {
		if err := SaveLocalSetting(ctx, dbh, k, v); err != nil {
			t.Fatal(err)
		}
	}
	// Update one.
	if err := SaveLocalSetting(ctx, dbh, settingBrightness, "0.6"); err != nil {
		t.Fatal(err)
	}

	s = LoadLocalSettings(ctx, dbh)
	if s[settingBrightness] != "0.6" {
		t.Errorf("brightness = %q, want 0.6 (update)", s[settingBrightness])
	}
	if len(s) != len(want) {
		t.Errorf("settings count = %d, want %d", len(s), len(want))
	}

	c := config.Default().Frame
	if err := applyLocalSettings(&c, s); err != nil {
		t.Fatal(err)
	}
	if c.Display.Brightness != 0.6 || c.Slideshow.Interval != 60*time.Second ||
		c.Slideshow.Transition != "slide" || c.Display.Orientation != "90" ||
		c.Sleep.On != "06:30" || c.Sleep.Off != "23:00" ||
		c.Slideshow.ShowCaption != false || c.Slideshow.ShowClock != true {
		t.Errorf("applyLocalSettings produced %+v", c)
	}
}

func TestApplyLocalSettingsRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		settingBrightness:        "0.9z",
		settingSlideshowInterval: "banana",
		settingTransition:        "warp",
		settingOrientation:       "45",
		settingShowCaption:       "maybe",
	}
	for k, v := range cases {
		c := config.Default().Frame
		if err := applyLocalSettings(&c, map[string]string{k: v}); err == nil {
			t.Errorf("%s=%q: expected error", k, v)
		}
	}
	// Interval below the 5s floor is rejected too.
	c := config.Default().Frame
	if err := applyLocalSettings(&c, map[string]string{settingSlideshowInterval: "1s"}); err == nil {
		t.Error("1s interval accepted, want rejection")
	}
}
