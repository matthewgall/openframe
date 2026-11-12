package frame

import (
	"context"
	"database/sql"
	"math/rand"
	"time"

	"github.com/matthewgall/openframe/internal/frame/display"
)

// slideshowComponent is the M3 headless simulator (spec §20:
// "Create a simulator for frame behaviour on Linux AMD64"). It
// presents the ACTIVE collection on the display driver at the
// configured slideshow interval, cycling through the photos.
//
// It reads only from the active collection (the photos table), which
// changes atomically at activation — so a renderer can never show a
// half-synced set, and a renderer failure cannot corrupt sync state
// (spec §7: "A renderer failure must not corrupt synchronisation
// state"). It runs whether or not the server is reachable: when no
// collection is active it keeps the driver on its last image (or
// black), which is the offline behaviour frames must have.
func (f *Frame) slideshowComponent(ctx context.Context) error {
	interval := f.cfg.Frame.Slideshow.Interval
	if interval < 5*time.Second {
		interval = 45 * time.Second
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	var idx int
	presented := 0
	for {
		if photos := f.activePhotos(ctx); len(photos) > 0 {
			// Order: sequential walks capture order; random shuffles
			// each cycle (M4 refines this into the full slideshow UI).
			var pick int
			if f.cfg.Frame.Slideshow.Order == "random" {
				pick = rng.Intn(len(photos))
			} else {
				pick = idx % len(photos)
				idx++
			}
			p := photos[pick]
			if p.displayPath != "" {
				ref := display.ImageRef{Path: p.displayPath, Hash: p.hash}
				if err := f.drv.Present(ctx, ref); err != nil {
					f.log.Warn("present failed (display keeps last image)",
						"photo", p.hash[:12], "error", err)
				} else {
					presented++
					f.log.Debug("presenting",
						"photo", p.hash[:12],
						"name", p.name,
						"index", pick,
						"of", len(photos),
					)
				}
			}
		} else {
			f.log.Debug("slideshow idle: no active collection")
		}
		select {
		case <-ctx.Done():
			f.log.Info("slideshow stopped", "presented", presented)
			return nil
		case <-time.After(interval + f.jitter(interval)):
		}
	}
}

// jitter adds a small deterministic-ish delay variance so a fleet of
// frames does not present in lockstep (battery/thermal smoothing).
func (f *Frame) jitter(base time.Duration) time.Duration {
	return time.Duration(float64(base) * 0.05) // +5%
}

type slideshowPhoto struct {
	hash        string
	name        string
	displayPath string
}

// activePhotos lists the active collection in display order: capture
// date (oldest first, unknowns last), then hash for stability.
func (f *Frame) activePhotos(ctx context.Context) []slideshowPhoto {
	rows, err := f.db.QueryContext(ctx,
		`SELECT hash, name, display_path
		   FROM photos
		  WHERE display_path != ''
		  ORDER BY
		    CASE WHEN capture_date IS NULL OR capture_date = '' THEN 1 ELSE 0 END,
		    capture_date,
		    hash`)
	if err != nil {
		f.log.Warn("active collection query failed", "error", err)
		return nil
	}
	defer rows.Close()
	var out []slideshowPhoto
	for rows.Next() {
		var p slideshowPhoto
		var dp sql.NullString
		if err := rows.Scan(&p.hash, &p.name, &dp); err != nil {
			return nil
		}
		p.displayPath = dp.String
		if p.displayPath != "" {
			out = append(out, p)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}
