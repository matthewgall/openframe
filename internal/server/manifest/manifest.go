// Package manifest builds the per-device synchronisation manifest
// (spec §9 "Manifest protocol").
//
// The manifest is deterministic for a given server state: albums are
// sorted by id, photos by content hash, and no wall-clock values are
// embedded in the content. Its canonical form is hashed to derive the
// manifest revision, so frames see a stable ETag while nothing changed
// and a new one the moment any assigned content changes.
//
// Manifests reference files by authenticated resource identifier
// (relative to the frame API), never by permanent object-storage URL
// (spec §9: "Do not expose permanent unrestricted object-storage
// URLs").
package manifest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/matthewgall/openframe/internal/protocol"
	"github.com/matthewgall/openframe/internal/server/devices"
	"github.com/matthewgall/openframe/internal/server/storage"
)

// File is one downloadable derivative inside a manifest.
type File struct {
	// Resource is the authenticated resource identifier, a single path
	// segment of the form "<hash>.<variant>". The frame downloads it
	// from /api/v1/frames/{device}/photos/<resource>. It is never a
	// permanent object-storage URL (spec §9).
	Resource string `json:"resource"`
	Size     int64  `json:"size_bytes"`
	// SHA256 is the integrity hash the frame must verify before
	// committing the file to its cache (spec §9 download safety).
	SHA256 string `json:"sha256"`
}

// Photo is one photograph in a manifest.
type Photo struct {
	Hash        string `json:"hash"`
	Name        string `json:"name"`
	Caption     string `json:"caption,omitempty"`
	CaptureDate string `json:"capture_date,omitempty"` // RFC3339
	Priority    bool   `json:"priority,omitempty"`
	Favourite   bool   `json:"favourite,omitempty"`
	Display     *File  `json:"display,omitempty"`
	Thumb       *File  `json:"thumb,omitempty"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// Album is an assigned collection inside a manifest.
type Album struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	CoverHash string  `json:"cover_hash,omitempty"`
	Priority  bool    `json:"priority,omitempty"`
	Photos    []Photo `json:"photos"`
}

// Config is the per-frame configuration carried in the manifest.
// M3 ships the device name; M5 adds the administrator-managed
// settings (spec §6). Zero/empty values are unset: the frame falls
// back to its local settings, then to its config file. Changes bump
// config_revision without churning photo downloads.
type Config struct {
	Name string `json:"name"`

	SlideshowIntervalSec int    `json:"slideshow_interval_sec,omitempty"`
	Transition           string `json:"transition,omitempty"` // none|crossfade|slide
	TransitionDurationMS int    `json:"transition_duration_ms,omitempty"`
	SyncIntervalSec      int    `json:"sync_interval_sec,omitempty"`
	SleepOn              string `json:"sleep_on,omitempty"`   // "HH:MM" or ""
	SleepOff             string `json:"sleep_off,omitempty"`  // "HH:MM" or ""
	Brightness           *int   `json:"brightness,omitempty"` // 0..100
	Orientation          string `json:"orientation,omitempty"`
	ShowClock            *bool  `json:"show_clock,omitempty"`
	ShowCaptions         *bool  `json:"show_captions,omitempty"`

	// UpdateChannel is the release channel for this frame's updater
	// (M6, spec §14). Empty means "not set"; the frame persists the
	// resolved value for the root updater to read.
	UpdateChannel string `json:"update_channel,omitempty"`
}

// Manifest is the wire document.
type Manifest struct {
	Protocol       string  `json:"protocol"`
	Revision       int64   `json:"revision"`
	ConfigRevision int64   `json:"config_revision"`
	Config         Config  `json:"config"`
	Albums         []Album `json:"albums"`
}

// ETag renders the manifest's HTTP ETag value. It covers BOTH the
// content revision and the config revision (M5): a config-only change
// must invalidate the frame's cached manifest so the new settings are
// delivered, without churning any photo downloads.
func ETag(revision, configRevision int64) string {
	return fmt.Sprintf("\"%d:%d\"", revision, configRevision)
}

// content is the deterministic subset of the manifest that the
// revision is derived from.
type content struct {
	Protocol string  `json:"protocol"`
	Albums   []Album `json:"albums"`
}

// Build computes the manifest for a device plus the new fingerprints
// (revisions) to persist. The input state is the device's stored
// fingerprints; revisions advance only when content changes.
// frameCfg is the administrator-managed per-frame configuration (M5);
// pass a zero FrameConfig when none is set.
func Build(
	ctx context.Context,
	dbh *sql.DB,
	store storage.Storage,
	deviceID string,
	state devices.ManifestState,
	frameCfg *devices.FrameConfig,
) (*Manifest, devices.ManifestState, error) {
	var devName string
	if err := dbh.QueryRowContext(ctx,
		"SELECT name FROM devices WHERE id = ?", deviceID).Scan(&devName); err != nil {
		if err == sql.ErrNoRows {
			return nil, state, devices.ErrNotFound
		}
		return nil, state, fmt.Errorf("manifest: load device: %w", err)
	}

	favs, err := loadFavourites(ctx, dbh, deviceID)
	if err != nil {
		return nil, state, err
	}

	albums, err := loadAlbums(ctx, dbh, store, deviceID, favs)
	if err != nil {
		return nil, state, err
	}
	if albums == nil {
		albums = []Album{}
	}

	c := content{Protocol: protocol.ManifestProtocol, Albums: albums}
	canonical, err := json.Marshal(c)
	if err != nil {
		return nil, state, fmt.Errorf("manifest: canonicalise: %w", err)
	}
	sum := sha256.Sum256(canonical)
	newHash := hex.EncodeToString(sum[:])

	newState := state
	if newHash != state.ManifestHash {
		newState.ManifestRevision = state.ManifestRevision + 1
		newState.ManifestHash = newHash
	}
	// Config revision is independent of content (a rename or a config
	// change must not churn photo downloads).
	mc := Config{Name: devName}
	if frameCfg != nil {
		mc.UpdateChannel = frameCfg.UpdateChannel
		mc.SlideshowIntervalSec = frameCfg.SlideshowIntervalSec
		mc.Transition = frameCfg.Transition
		mc.TransitionDurationMS = frameCfg.TransitionDurationMS
		mc.SyncIntervalSec = frameCfg.SyncIntervalSec
		mc.SleepOn = frameCfg.SleepOn
		mc.SleepOff = frameCfg.SleepOff
		mc.Brightness = frameCfg.Brightness
		mc.Orientation = frameCfg.Orientation
		mc.ShowClock = frameCfg.ShowClock
		mc.ShowCaptions = frameCfg.ShowCaptions
	}
	cfg, err := json.Marshal(mc)
	if err != nil {
		return nil, state, err
	}
	cfgSum := sha256.Sum256(cfg)
	cfgHash := hex.EncodeToString(cfgSum[:])
	if cfgHash != state.ConfigHash {
		newState.ConfigRevision = state.ConfigRevision + 1
		newState.ConfigHash = cfgHash
	}

	m := &Manifest{
		Protocol:       protocol.ManifestProtocol,
		Revision:       newState.ManifestRevision,
		ConfigRevision: newState.ConfigRevision,
		Config:         mc,
		Albums:         albums,
	}
	return m, newState, nil
}

// loadFavourites returns the device's favourite map.
func loadFavourites(ctx context.Context, dbh *sql.DB, deviceID string) (map[string]bool, error) {
	rows, err := dbh.QueryContext(ctx,
		"SELECT photo_hash, value FROM favourites WHERE device_id = ?", deviceID)
	if err != nil {
		return nil, fmt.Errorf("manifest: load favourites: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var h string
		var v int
		if err := rows.Scan(&h, &v); err != nil {
			return nil, err
		}
		out[h] = v == 1
	}
	return out, rows.Err()
}

// loadAlbums assembles the assigned, non-deleted albums and their
// non-deleted photos in deterministic order.
func loadAlbums(
	ctx context.Context,
	dbh *sql.DB,
	store storage.Storage,
	deviceID string,
	favs map[string]bool,
) ([]Album, error) {
	rows, err := dbh.QueryContext(ctx,
		`SELECT a.id, a.name, a.cover_photo_id, a.priority
		   FROM albums a
		   JOIN album_devices ad ON ad.album_id = a.id
		  WHERE ad.device_id = ? AND a.deleted_at IS NULL
		  ORDER BY a.id`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("manifest: load albums: %w", err)
	}
	defer rows.Close()

	type albumRow struct {
		id, name, coverID string
		priority          int
	}
	var albumRows []albumRow
	for rows.Next() {
		var r albumRow
		var cover sql.NullString
		if err := rows.Scan(&r.id, &r.name, &cover, &r.priority); err != nil {
			return nil, err
		}
		r.coverID = cover.String
		albumRows = append(albumRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	albums := make([]Album, 0, len(albumRows))
	for _, r := range albumRows {
		photos, err := loadPhotos(ctx, dbh, store, r.id, r.priority, favs)
		if err != nil {
			return nil, err
		}
		a := Album{
			ID:       r.id,
			Name:     r.name,
			Priority: r.priority == 1,
			Photos:   photos,
		}
		if a.Photos == nil {
			a.Photos = []Photo{}
		}
		// Resolve the cover (stored as a photo id) to the content hash
		// the frame's cache understands; empty when the cover photo was
		// deleted or removed from the album.
		if r.coverID != "" {
			if ch, err := CoverHashFor(ctx, dbh, r.coverID); err == nil {
				for _, p := range a.Photos {
					if p.Hash == ch {
						a.CoverHash = ch
						break
					}
				}
			}
		}
		albums = append(albums, a)
	}
	return albums, nil
}

// loadPhotos lists an album's live photos with their derivative file
// descriptors, sorted by hash for determinism.
func loadPhotos(
	ctx context.Context,
	dbh *sql.DB,
	store storage.Storage,
	albumID string,
	albumPriority int,
	favs map[string]bool,
) ([]Photo, error) {
	rows, err := dbh.QueryContext(ctx,
		`SELECT p.hash, p.original_name, p.caption, p.capture_date, p.width, p.height,
		        p.priority, p.id
		   FROM photos p
		   JOIN album_photos ap ON ap.photo_id = p.id
		  WHERE ap.album_id = ? AND p.deleted_at IS NULL
		  ORDER BY p.hash`, albumID)
	if err != nil {
		return nil, fmt.Errorf("manifest: load photos: %w", err)
	}
	defer rows.Close()

	var photos []Photo
	for rows.Next() {
		var (
			p       Photo
			capture sql.NullString
			prio    int
			photoID string
		)
		if err := rows.Scan(&p.Hash, &p.Name, &p.Caption, &capture, &p.Width, &p.Height, &prio, &photoID); err != nil {
			return nil, err
		}
		p.CaptureDate = capture.String
		p.Priority = prio == 1 || albumPriority == 1
		p.Favourite = favs[p.Hash]

		display, err := fileFor(ctx, dbh, store, p.Hash, storage.VariantDisplay)
		if err != nil {
			return nil, err
		}
		thumb, err := fileFor(ctx, dbh, store, p.Hash, storage.VariantThumb)
		if err != nil {
			return nil, err
		}
		p.Display, p.Thumb = display, thumb
		photos = append(photos, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return photos, nil
}

// fileFor describes a derivative if it exists in storage; nil when the
// object is missing (the frame then skips that file and the manifest
// still validates — missing derivatives are a server-side repair
// condition, not a frame failure). The integrity hash comes from the
// photo_derivatives table, backfilled on the fly for photos ingested
// before M3.
func fileFor(ctx context.Context, dbh *sql.DB, store storage.Storage, hash, variant string) (*File, error) {
	key := storage.Key(hash + "." + variant)
	exists, err := store.Exists(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("manifest: stat %s: %w", key, err)
	}
	if !exists {
		return nil, nil
	}
	size, err := store.Size(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("manifest: size %s: %w", key, err)
	}
	sha, _, ok, err := derivativeHash(ctx, dbh, hash, variant)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Legacy photo: hash the stored object and record it.
		sha, err = hashObject(ctx, store, key)
		if err != nil {
			return nil, err
		}
		if err := recordDerivative(ctx, dbh, hash, variant, sha, size); err != nil {
			return nil, err
		}
	}
	return &File{
		Resource: hash + "." + variant,
		Size:     size,
		SHA256:   sha,
	}, nil
}

func derivativeHash(ctx context.Context, dbh *sql.DB, hash, variant string) (string, int64, bool, error) {
	var sha string
	var size int64
	err := dbh.QueryRowContext(ctx,
		"SELECT sha256, size_bytes FROM photo_derivatives WHERE photo_hash = ? AND variant = ?",
		hash, variant).Scan(&sha, &size)
	if err == sql.ErrNoRows {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("manifest: derivative hash: %w", err)
	}
	return sha, size, true, nil
}

func recordDerivative(ctx context.Context, dbh *sql.DB, hash, variant, sha string, size int64) error {
	_, err := dbh.ExecContext(ctx,
		`INSERT INTO photo_derivatives (photo_hash, variant, sha256, size_bytes, generated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(photo_hash, variant) DO NOTHING`,
		hash, variant, sha, size, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("manifest: record derivative: %w", err)
	}
	return nil
}

func hashObject(ctx context.Context, store storage.Storage, key storage.Key) (string, error) {
	rc, err := store.Open(ctx, key)
	if err != nil {
		return "", fmt.Errorf("manifest: open %s: %w", key, err)
	}
	defer rc.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, rc); err != nil {
		return "", fmt.Errorf("manifest: hash %s: %w", key, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// CoverHashFor resolves an album's cover photo id to its content hash
// (exported for tests).
func CoverHashFor(ctx context.Context, dbh *sql.DB, photoID string) (string, error) {
	var hash string
	err := dbh.QueryRowContext(ctx,
		"SELECT hash FROM photos WHERE id = ?", photoID).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return hash, nil
}
