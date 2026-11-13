//go:build linux

package drm

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/matthewgall/openframe/internal/frame/display"
)

// The DRM backend registers itself with the display factory from
// init: the factory can then construct it by name without this
// package being imported by the factory package (which would be an
// import cycle, since we implement display.Driver).
func init() {
	display.Register("drm", func(ctx context.Context, log *slog.Logger, opts display.Options) (display.Driver, error) {
		card := opts.Card
		if card == "" {
			card = "/dev/dri/card0"
		}
		w, h := opts.Width, opts.Height
		// Let the panel decide the resolution when a panel is
		// attached: the canvas must match the mode exactly.
		if pw, ph, err := PreferredModeSize(card); err == nil && pw > 0 && ph > 0 {
			if pw != w || ph != h {
				log.Info("drm: using panel resolution", "card", card, "width", pw, "height", ph)
			}
			w, h = pw, ph
		}
		d, err := New(log, card, w, h)
		if err != nil {
			return nil, fmt.Errorf("drm: %w", err)
		}
		if err := d.Init(ctx); err != nil {
			d.Close()
			return nil, fmt.Errorf("drm init: %w", err)
		}
		return d, nil
	})
}
