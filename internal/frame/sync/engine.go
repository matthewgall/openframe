// Package sync is the frame's offline-first synchronisation engine
// (spec §8, §9).
//
// Design (spec §7, §9):
//   - Responsibilities are separated and independently scheduled:
//     manifest checking, photograph downloading, collection
//     activation and local-change upload each run as their own
//     component with their own cadence; the M6 update coordinator will
//     slot in as a fifth.
//   - The engine owns all network state. A renderer failure cannot
//     corrupt it, and a sync failure cannot take the display down: the
//     display always renders the last safely-activated collection.
//   - Bounded everything: a fixed worker set per batch (cfg
//     MaxConcurrent), per-file size caps, a bounded manifest body, and
//     no unbounded goroutine creation.
//
// Offline behaviour: when the server is unreachable the engine logs
// the attempt, backs off exponentially with jitter, and stops touching
// the cache. The frame keeps displaying the active collection
// indefinitely (spec §9: "If the server disappears, continue
// displaying cached photographs").
package sync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/frame/cache"
	"github.com/matthewgall/openframe/internal/logging"
)

// Client-facing manifest types (frame side; mirror the server's wire
// format in internal/server/manifest).
type manifest struct {
	Protocol       string          `json:"protocol"`
	Revision       int64           `json:"revision"`
	ConfigRevision int64           `json:"config_revision"`
	Config         Config          `json:"config"`
	Albums         []manifestAlbum `json:"albums"`
}

// Config is the per-frame configuration pushed by the server in the
// manifest (M5, spec §6). Zero/empty values are unset: the frame falls
// back to its local settings, then to its config file.
type Config struct {
	Name                 string `json:"name"`
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

	// UpdateChannel (M6, spec §14): the release channel this frame
	// updates from, administered in the admin UI. Empty = unset.
	UpdateChannel string `json:"update_channel,omitempty"`
}

type manifestAlbum struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	CoverHash string          `json:"cover_hash,omitempty"`
	Priority  bool            `json:"priority,omitempty"`
	Photos    []manifestPhoto `json:"photos"`
}

type manifestPhoto struct {
	Hash        string        `json:"hash"`
	Name        string        `json:"name"`
	Caption     string        `json:"caption,omitempty"`
	CaptureDate string        `json:"capture_date,omitempty"`
	Priority    bool          `json:"priority,omitempty"`
	Favourite   bool          `json:"favourite,omitempty"`
	Display     *manifestFile `json:"display,omitempty"`
	Thumb       *manifestFile `json:"thumb,omitempty"`
	Width       int           `json:"width"`
	Height      int           `json:"height"`
}

type manifestFile struct {
	Resource string `json:"resource"`
	Size     int64  `json:"size_bytes"`
	SHA256   string `json:"sha256"`
}

// ChangeOp is one queued local change (frame -> server).
type ChangeOp struct {
	OpID      string `json:"op_id"`
	Type      string `json:"type"`
	PhotoHash string `json:"photo_hash"`
	Value     *bool  `json:"value,omitempty"`
	TS        string `json:"ts"`
}

// ChangeResult is the server's ingest receipt. RejectedOps names the
// ops the server will not apply; every op not named is applied
// (accepted or already-known duplicate) and safe to ack.
type ChangeResult struct {
	Accepted    int      `json:"accepted"`
	Duplicate   int      `json:"duplicates"`
	Rejected    int      `json:"rejected"`
	RejectedOps []string `json:"rejected_ops,omitempty"`
}

// sync-state keys (frame.db sync_state table).
const (
	keyLastAttempt         = "last_attempt"
	keyLastSuccess         = "last_success"
	keyLastError           = "last_error"
	keyConsecutiveFailures = "consecutive_failures"
	keyLastETag            = "last_manifest_etag"
	keyActiveRevision      = "active_revision"
	keyDeviceName          = "device_name"
	keyConfigRevision      = "config_revision"
)

// Engine is the frame synchronisation engine.
type Engine struct {
	log       *slog.Logger
	db        *sql.DB
	cache     *cache.Store
	client    *Client
	serverURL string
	deviceID  string
	syncCfg   config.SyncConfig
	// math/rand.Rand is not safe for concurrent use; the download
	// workers share the engine, so every draw is mutex-guarded.
	rngMu sync.Mutex
	rng   *rand.Rand

	// onActivate is called (synchronously, on the download loop's
	// goroutine) after a collection has been committed active. The
	// M4 UI uses it to reload its data; the callback must be fast and
	// non-blocking.
	activateMu sync.Mutex
	onActivate func(revision int64)

	// onConfig is called (synchronously, on the goroutine that ran
	// Check) when the server's per-frame configuration advances.
	configMu sync.Mutex
	onConfig func(Config)

	// checkMu serialises Check: the manifest loop and the M5
	// remote-sync trigger (heartbeat) can both request a check.
	checkMu sync.Mutex

	// manifestIntervalOverride is the server-configured manifest
	// poll interval in seconds (0 = use the config file value).
	manifestIntervalOverride atomic.Int64
}

// New wires an engine. An empty serverURL leaves the engine in the
// "no server configured" state (unpaired; it idles without dialling)
// so a not-yet-provisioned frame still runs its display.
func New(deviceID, serverURL, token string, log *slog.Logger, dbh *sql.DB, c *cache.Store, cfg *config.FrameConfig) (*Engine, error) {
	if dbh == nil || c == nil || cfg == nil {
		return nil, errors.New("sync: nil dependency")
	}
	if log == nil {
		log = slog.Default()
	}
	log = logging.NewComponent(log, "sync")
	e := &Engine{
		log: log, db: dbh, cache: c, deviceID: deviceID,
		serverURL: serverURL, syncCfg: cfg.Sync,
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	if serverURL == "" {
		return e, nil
	}
	cl, err := NewClient(serverURL, token)
	if err != nil {
		return nil, err
	}
	e.client = cl
	return e, nil
}

// Paired reports whether the engine has a server and a credential.
func (e *Engine) Paired() bool { return e.client != nil && e.client.token != "" }

// SetCredential installs (or replaces) the device credential, e.g. after
// the M5 pairing flow completes. The frame identity (device ID) is
// unchanged: pairing registers the frame under its own identity.
func (e *Engine) SetCredential(token string) error {
	if e.serverURL == "" {
		return errors.New("sync: no server configured")
	}
	if token == "" {
		return errors.New("sync: empty credential")
	}
	cl, err := NewClient(e.serverURL, token)
	if err != nil {
		return err
	}
	e.client = cl
	return nil
}

// ServerURL returns the configured server base URL ("" when unset).
func (e *Engine) ServerURL() string { return e.serverURL }

// OnConfig registers a callback fired when the server's per-frame
// configuration advances (M5). It runs on the goroutine that performed
// the manifest check; the callback must be fast and non-blocking.
func (e *Engine) OnConfig(fn func(Config)) {
	e.configMu.Lock()
	e.onConfig = fn
	e.configMu.Unlock()
}

func (e *Engine) fireConfig(cfg Config) {
	e.configMu.Lock()
	fn := e.onConfig
	e.configMu.Unlock()
	if fn != nil {
		fn(cfg)
	}
}

// SetManifestInterval overrides the manifest poll interval (seconds),
// 0 restores the config-file value. Called by the M5 config applier.
func (e *Engine) SetManifestInterval(seconds int) {
	if seconds > 0 {
		e.manifestIntervalOverride.Store(int64(seconds))
	} else {
		e.manifestIntervalOverride.Store(0)
	}
}

// DeviceID returns the frame identity the engine authenticates as.
func (e *Engine) DeviceID() string { return e.deviceID }

// ReportHealth posts a health report (M5, spec §13) and returns the
// server's remote-sync request sequence. Delegates to the client so
// the credential is applied exactly once.
func (e *Engine) ReportHealth(ctx context.Context, payload []byte) (int64, error) {
	if err := e.client.requirePairing(); err != nil {
		return 0, err
	}
	return e.client.ReportHealth(ctx, e.deviceID, payload)
}

// LastAttempt returns the time of the most recent manifest check.
func (e *Engine) LastAttempt(ctx context.Context) (time.Time, bool) {
	s := e.getState(ctx, keyLastAttempt)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// LastSync returns the time of the last successful manifest activation
// and whether one has happened.
func (e *Engine) LastSync(ctx context.Context) (time.Time, bool) {
	s := e.getState(ctx, keyLastSuccess)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// LastError returns the last sync error summary ("" when healthy).
func (e *Engine) LastError(ctx context.Context) string { return e.getState(ctx, keyLastError) }

// ActiveRevision returns the active collection revision.
func (e *Engine) ActiveRevision(ctx context.Context) int64 {
	v, _ := parseI64(e.getState(ctx, keyActiveRevision))
	return v
}

// PendingDownloads returns the number of queued downloads.
func (e *Engine) PendingDownloads(ctx context.Context) int {
	var n int
	_ = e.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM download_queue").Scan(&n)
	return n
}

// OnActivate registers a callback fired after each successful
// activation with the new collection revision. It must be called
// before the loops start (or otherwise synchronized with them).
func (e *Engine) OnActivate(fn func(revision int64)) {
	e.activateMu.Lock()
	e.onActivate = fn
	e.activateMu.Unlock()
}

func (e *Engine) fireActivate(rev int64) {
	e.activateMu.Lock()
	fn := e.onActivate
	e.activateMu.Unlock()
	if fn != nil {
		fn(rev)
	}
}

// ServerConfigured reports whether a server URL is set (even if not
// yet paired).
func (e *Engine) ServerConfigured() bool { return e.client != nil }

// Components returns the independently scheduled background loops
// (spec §9: manifest check, download, local-change upload; activation
// rides on the download loop because it is the point where "all
// mandatory files verified" becomes true).
func (e *Engine) Components() []Component {
	return []Component{
		{Name: "sync.manifest", Run: e.ManifestLoop},
		{Name: "sync.downloads", Run: e.DownloadLoop},
		{Name: "sync.changes", Run: e.ChangesLoop},
	}
}

// Component is a named background loop (mirrors frame.Component so
// the CLI can pass engine loops into the frame runner without a
// sync->frame import cycle).
type Component struct {
	Name string
	Run  func(ctx context.Context) error
}

// ---------------------------------------------------------------------
// Sync-state helpers (frame.db sync_state).

func (e *Engine) setState(ctx context.Context, key, value string) {
	if _, err := e.db.ExecContext(ctx,
		`INSERT INTO sync_state (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		e.log.Warn("sync_state write failed", "key", key, "error", err)
	}
}

func (e *Engine) getState(ctx context.Context, key string) string {
	var v string
	if err := e.db.QueryRowContext(ctx,
		"SELECT value FROM sync_state WHERE key = ?", key).Scan(&v); err != nil {
		return ""
	}
	return v
}

// ---------------------------------------------------------------------
// 1. Manifest checker.

// ManifestLoop checks for a new manifest on the configured interval
// (with jitter) and, on a new revision, stages the collection. The
// loop stays up while unpaired (polling at the short unpaired tick) so
// a credential installed at runtime by the M5 pairing flow is picked
// up without restarting the frame.
func (e *Engine) ManifestLoop(ctx context.Context) error {
	for {
		if e.Paired() {
			e.Check(ctx)
		}
		delay := e.manifestDelay()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// manifestDelay returns the next poll delay: the server-configured or
// config-file interval with jitter when paired, a short unpaired tick
// otherwise (so a fresh pairing is noticed quickly).
func (e *Engine) manifestDelay() time.Duration {
	if !e.Paired() {
		return 30 * time.Second
	}
	if v := e.manifestIntervalOverride.Load(); v > 0 {
		// Server-configured interval (M5), in seconds.
		base := time.Duration(v) * time.Second
		return base + e.jitter(base, 0.2)
	}
	base := e.syncCfg.ManifestInterval
	if base <= 0 {
		base = 60 * time.Minute
	}
	delay := base + e.jitter(base, 0.2)
	if delay < 0 {
		delay = base
	}
	return delay
}

// Check performs one conditional manifest fetch and, on a new
// revision, stages the collection. It is the unit the manifest loop,
// the M5 remote-sync trigger and tests call.
func (e *Engine) Check(ctx context.Context) {
	// checkMu serialises the manifest loop with the M5 remote-sync
	// trigger (both can call Check concurrently).
	e.checkMu.Lock()
	defer e.checkMu.Unlock()

	start := time.Now()
	e.setState(ctx, keyLastAttempt, start.UTC().Format(time.RFC3339Nano))

	res, err := e.client.GetManifest(ctx, e.deviceID, e.getState(ctx, keyLastETag))
	if err != nil {
		e.failAttempt(ctx, err)
		return
	}
	e.setState(ctx, keyLastError, "")

	if !res.Modified {
		e.log.Debug("manifest unchanged", "etag", res.ETag)
		e.resetFailures(ctx)
		return
	}

	m, staged, err := e.stageManifest(ctx, res.Body)
	if err != nil {
		e.failAttempt(ctx, err)
		return
	}
	e.setState(ctx, keyLastETag, res.ETag)
	e.resetFailures(ctx)
	if m == nil {
		return
	}
	e.applyConfig(ctx, m)
	if staged {
		e.log.Info("manifest staged",
			"revision", m.Revision,
			"albums", len(m.Albums),
			"elapsed_ms", time.Since(start).Milliseconds(),
		)
	}
}

// applyConfig fires the OnConfig callback when the server's per-frame
// configuration has advanced. A config-only change does not bump the
// content revision, but the manifest ETag covers config_revision too,
// so the frame re-fetches and sees it.
func (e *Engine) applyConfig(ctx context.Context, m *manifest) {
	last, _ := parseI64(e.getState(ctx, keyConfigRevision))
	if m.ConfigRevision <= last {
		return
	}
	e.setState(ctx, keyConfigRevision, fmt.Sprint(m.ConfigRevision))
	e.log.Info("frame config updated",
		"config_revision", m.ConfigRevision, "name", m.Config.Name)
	e.fireConfig(m.Config)
}

// stageManifest persists a new manifest as a STAGING collection and
// populates the download queue. The active tables (photos, albums)
// are left untouched until activation, so the renderer keeps showing
// the previous collection until every mandatory file is verified
// (spec §8). Single transaction: no half-staged collection.
// Returns the parsed manifest and whether a new staging was created
// (false for a config-only update at the same content revision).
func (e *Engine) stageManifest(ctx context.Context, body []byte) (*manifest, bool, error) {
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Protocol == "" {
		return nil, false, errors.New("manifest: missing protocol")
	}
	if m.Revision <= 0 {
		return nil, false, fmt.Errorf("manifest: invalid revision %d", m.Revision)
	}
	// Monotonicity guard: a revision at or below the active one is a
	// stale replay (or a server rollback); ignore it so we never
	// downgrade the displayed collection.
	activeRev, _ := parseI64(e.getState(ctx, keyActiveRevision))
	if m.Revision <= activeRev {
		e.log.Info("manifest revision not newer; ignoring",
			"got", m.Revision, "active", activeRev)
		// The parsed manifest still carries the server config
		// (e.g. a config-only update): return it, unstaged.
		return &m, false, nil
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Replace any previous staging (its queue + staging rows cascade).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM collections WHERE state = 'staging'`); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO collections (revision, photo_count, total_bytes, state, created_at)
		 VALUES (?, 0, 0, 'staging', ?)`, m.Revision, now); err != nil {
		return nil, false, err
	}

	totalBytes := int64(0)
	queued := 0
	photoCount := 0
	seen := map[string]bool{}

	// 1. Staging albums.
	for _, a := range m.Albums {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO staging_albums (collection_revision, id, name, cover_hash)
			 VALUES (?, ?, ?, ?)`,
			m.Revision, a.ID, a.Name, nullIfEmpty(a.CoverHash)); err != nil {
			return nil, false, fmt.Errorf("stage album %s: %w", a.ID, err)
		}
	}

	// 2. Staging photos; queue the missing files.
	for _, a := range m.Albums {
		for _, p := range a.Photos {
			if seen[p.Hash] {
				continue // same original in two albums: one cache copy
			}
			seen[p.Hash] = true
			photoCount++

			haveDisplay := e.cacheCached(p.Hash, cache.VariantDisplay, p.Display)
			haveThumb := e.cacheCached(p.Hash, cache.VariantThumb, p.Thumb)
			if p.Display != nil && !haveDisplay {
				totalBytes += p.Display.Size
			}
			if p.Thumb != nil && !haveThumb {
				totalBytes += p.Thumb.Size
			}
			disSHA, disRes := "", ""
			if p.Display != nil {
				disSHA, disRes = p.Display.SHA256, p.Display.Resource
			}
			thSHA, thRes := "", ""
			if p.Thumb != nil {
				thSHA, thRes = p.Thumb.SHA256, p.Thumb.Resource
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO staging_photos (collection_revision, hash, album_id, name, caption,
				                             capture_date, display_bytes, thumb_bytes,
				                             display_sha, thumb_sha, display_resource, thumb_resource,
				                             priority, is_favourite)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				m.Revision, p.Hash, a.ID, p.Name, p.Caption, nullIfEmpty(p.CaptureDate),
				int64Or0(p.Display), int64Or0(p.Thumb),
				nullIfEmpty(disSHA), nullIfEmpty(thSHA), nullIfEmpty(disRes), nullIfEmpty(thRes),
				boolInt(p.Priority), boolInt(p.Favourite)); err != nil {
				return nil, false, fmt.Errorf("stage photo %s: %w", p.Hash, err)
			}
			// Server-authoritative favourite (no pending local op).
			e.applyServerFavourite(tx, ctx, p)

			if p.Display != nil && !haveDisplay {
				e.enqueue(tx, ctx, m.Revision, p.Hash, cache.VariantDisplay, p.Display.Size, disSHA, disRes, &queued)
			}
			if p.Thumb != nil && !haveThumb {
				e.enqueue(tx, ctx, m.Revision, p.Hash, cache.VariantThumb, p.Thumb.Size, thSHA, thRes, &queued)
			}
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE collections SET photo_count = ?, total_bytes = ? WHERE revision = ? AND state = 'staging'`,
		photoCount, totalBytes, m.Revision); err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	// Per-frame configuration (M3: device name for the M4 settings
	// screen; M5 adds settings). Stored so the display can use it
	// offline.
	if m.Config.Name != "" {
		e.setState(ctx, keyDeviceName, m.Config.Name)
	}
	e.log.Debug("collection staged",
		"revision", m.Revision, "photos", photoCount, "queued", queued, "bytes", totalBytes)
	return &m, true, nil
}

// cacheCached reports whether a manifest file is already committed in
// the cache (and the manifest size matches — a mismatch means a
// corrupt/stale file and forces a redownload).
func (e *Engine) cacheCached(hash, variant string, f *manifestFile) bool {
	if f == nil {
		return true // no file required
	}
	if !e.cache.Exists(hash, variant) {
		return false
	}
	size, err := e.cache.Size(hash, variant)
	return err == nil && size == f.Size
}

func int64Or0(f *manifestFile) int64 {
	if f == nil {
		return 0
	}
	return f.Size
}

// enqueue adds a download-queue row for the staging collection.
func (e *Engine) enqueue(tx *sql.Tx, ctx context.Context, revision int64, hash, variant string, size int64, sha, resource string, queued *int) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO download_queue (collection_revision, hash, variant, bytes, sha256, resource, state)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending')
		 ON CONFLICT(collection_revision, hash, variant) DO UPDATE SET
		   bytes = excluded.bytes, sha256 = excluded.sha256,
		   resource = excluded.resource, state = 'pending'`,
		revision, hash, variant, size, sha, resource)
	if err != nil {
		e.log.Warn("enqueue failed", "hash", hash, "variant", variant, "error", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		*queued++
	}
}

// applyServerFavourite adopts the server's favourite state when there
// is no pending local operation for the photo (offline local changes
// win until they are acknowledged — spec §5 conflict rule).
func (e *Engine) applyServerFavourite(tx *sql.Tx, ctx context.Context, p manifestPhoto) {
	var pending int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM favourites_queue WHERE photo_hash = ?", p.Hash).Scan(&pending); err != nil || pending > 0 {
		return
	}
	v := 0
	if p.Favourite {
		v = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO favourites (photo_hash, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(photo_hash) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		p.Hash, v, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		e.log.Warn("apply server favourite failed", "hash", p.Hash, "error", err)
	}
}

func (e *Engine) failAttempt(ctx context.Context, err error) {
	msg := truncate(err.Error(), 300)
	e.setState(ctx, keyLastError, msg)
	failures := e.consecutiveFailures(ctx) + 1
	e.setState(ctx, keyConsecutiveFailures, fmt.Sprint(failures))
	var down bool
	var eDown *ErrServerDown
	if errors.As(err, &eDown) {
		down = true
	}
	e.log.Warn("sync attempt failed",
		"error", msg, "consecutive", failures, "server_down", down)
}

func (e *Engine) resetFailures(ctx context.Context) {
	e.setState(ctx, keyConsecutiveFailures, "0")
}

func (e *Engine) consecutiveFailures(ctx context.Context) int {
	n, _ := parseI64(e.getState(ctx, keyConsecutiveFailures))
	return int(n)
}

// ---------------------------------------------------------------------
// 2. Download processor (with activation).

// downloadTick is how often the download loop re-evaluates due work.
const downloadTick = 30 * time.Second

// DownloadLoop runs due downloads under the configured policy, then
// activates the staging collection when all its files are verified.
func (e *Engine) DownloadLoop(ctx context.Context) error {
	for {
		if e.Paired() {
			e.DownloadOnce(ctx)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(downloadTick):
		}
	}
}

// DownloadOnce processes one batch of due downloads and, if the
// staging collection is complete, activates it. Returns the number of
// files committed and the first per-file error (if any).
func (e *Engine) DownloadOnce(ctx context.Context) (int, error) {
	if !e.policyAllowsNow() {
		return 0, nil
	}
	rows, err := e.dueDownloads(ctx)
	if err != nil {
		return 0, err
	}
	if len(rows) > 0 {
		conc := e.syncCfg.MaxConcurrent
		if conc <= 0 {
			conc = 2
		}
		committed, firstErr := e.runBatch(ctx, rows, conc)
		if firstErr != nil {
			return committed, firstErr
		}
	}
	e.maybeActivate(ctx)
	return 0, nil
}

// runBatch downloads rows with a bounded worker pool (at most conc
// goroutines, all joined before return — no unbounded creation).
func (e *Engine) runBatch(ctx context.Context, rows []qRow, conc int) (int, error) {
	sem := make(chan struct{}, conc)
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		committed int
		firstErr  error
	)
	for _, row := range rows {
		row := row
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := e.downloadOne(ctx, row); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			committed++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return committed, firstErr
}

// qRow is a due download-queue entry.
type qRow struct {
	ID       int64
	Revision int64
	Hash     string
	Variant  string
	Bytes    int64
	SHA      string
	Resource string
	Attempts int
}

func (e *Engine) dueDownloads(ctx context.Context) ([]qRow, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := e.db.QueryContext(ctx,
		`SELECT dq.id, dq.collection_revision, dq.hash, dq.variant, dq.bytes,
		        dq.sha256, dq.resource, dq.attempts
		   FROM download_queue dq
		   JOIN collections c ON c.revision = dq.collection_revision
		  WHERE c.state = 'staging'
		    AND (dq.state = 'pending' OR dq.state = 'failed')
		    AND (dq.next_attempt IS NULL OR dq.next_attempt <= ?)
		  ORDER BY
		    (SELECT sp.priority FROM staging_photos sp
		        WHERE sp.hash = dq.hash AND sp.collection_revision = dq.collection_revision) DESC,
		    dq.id
		  LIMIT 64`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []qRow
	for rows.Next() {
		var r qRow
		if err := rows.Scan(&r.ID, &r.Revision, &r.Hash, &r.Variant, &r.Bytes,
			&r.SHA, &r.Resource, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// downloadOne fetches, verifies and commits one file (spec §9
// "Download safety", steps 1-7).
func (e *Engine) downloadOne(ctx context.Context, row qRow) error {
	// Pre-flight disk-space check (spec §8: quotas + disk monitoring).
	if ok, err := e.cache.HasRoom(row.Bytes); err == nil && !ok {
		return e.failRow(ctx, row, "disk full or quota exceeded; stopping downloads safely")
	}
	if e.syncCfg.MaxDownloadBytes > 0 && row.Bytes > e.syncCfg.MaxDownloadBytes {
		return e.failRowPermanent(ctx, row, "file exceeds max_download_bytes")
	}

	// Resume: a partial .part from a previous attempt is continued with
	// a Range request. An oversized/stale partial restarts from zero.
	offset, _ := e.cache.PartSize(row.Hash, row.Variant)
	if offset >= row.Bytes {
		offset = 0
		if err := e.cache.RemovePart(row.Hash, row.Variant); err != nil {
			return err
		}
	}

	fl, err := os.OpenFile(e.cache.PartPath(row.Hash, row.Variant), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return e.failRow(ctx, row, "open temp: "+err.Error())
	}
	defer fl.Close()
	if offset > 0 {
		if _, err := fl.Seek(offset, io.SeekStart); err != nil {
			return e.failRow(ctx, row, "seek temp: "+err.Error())
		}
	}

	written, partial, err := e.client.OpenDownload(ctx, e.deviceID, row.Resource, offset, fl, row.Bytes)
	if err != nil {
		var eDown *ErrServerDown
		if errors.As(err, &eDown) {
			return e.failRow(ctx, row, "server unreachable")
		}
		return e.failRow(ctx, row, "download: "+err.Error())
	}
	// Server ignored our Range (200 on a resumed request): the .part
	// now holds a fresh full body, not an extension. Discard and retry
	// from zero on the next tick (the part is oversized/invalid).
	if offset > 0 && !partial {
		_ = e.cache.RemovePart(row.Hash, row.Variant)
		return e.failRow(ctx, row, "server did not honour Range; restarting")
	}
	if offset+written != row.Bytes {
		return e.failRow(ctx, row, fmt.Sprintf("short read: %d of %d bytes", offset+written, row.Bytes))
	}

	// Flush before verifying/committing (spec §9 step 5).
	if err := fl.Sync(); err != nil {
		return e.failRow(ctx, row, "sync: "+err.Error())
	}
	fl.Close()

	// Verify SHA-256 (spec §9 step 4). Mismatch: discard, retry (a
	// transient storage glitch server-side is the likely cause).
	if got := hashFile(e.cache.PartPath(row.Hash, row.Variant)); got != row.SHA {
		_ = e.cache.RemovePart(row.Hash, row.Variant)
		return e.failRow(ctx, row, "sha256 mismatch (file discarded)")
	}

	if err := e.cache.Commit(row.Hash, row.Variant); err != nil {
		return e.failRow(ctx, row, "commit: "+err.Error())
	}

	// Transactional metadata update (spec §9 steps 6-7): photo row and
	// queue row move together.
	if err := e.commitRow(ctx, row); err != nil {
		return err
	}
	e.log.Debug("file verified + committed",
		"hash", row.Hash[:12], "variant", row.Variant, "bytes", row.Bytes)
	return nil
}

// commitRow marks a queue row complete (spec §9 steps 6-7; the photo
// metadata merges into the active collection at activation, where the
// cache paths are written atomically).
func (e *Engine) commitRow(ctx context.Context, row qRow) error {
	_, err := e.db.ExecContext(ctx, "DELETE FROM download_queue WHERE id = ?", row.ID)
	return err
}

// failRow records a transient failure with exponential backoff
// (base 30s, x2 per attempt, cap 1h, +-25% jitter). The .part is kept
// for resume.
func (e *Engine) failRow(ctx context.Context, row qRow, msg string) error {
	next := backoff(row.Attempts+1, 30*time.Second, time.Hour, e.randFloat64)
	_, err := e.db.ExecContext(ctx,
		`UPDATE download_queue
		    SET state = 'failed', attempts = attempts + 1,
		        next_attempt = ?, last_error = ?
		  WHERE id = ?`,
		time.Now().UTC().Add(next).Format(time.RFC3339Nano), truncate(msg, 200), row.ID)
	if err != nil {
		return err
	}
	e.log.Warn("download failed (will retry)",
		"hash", row.Hash[:12], "variant", row.Variant,
		"attempt", row.Attempts+1, "next_in", next, "error", msg)
	return nil
}

// failRowPermanent records a failure that will not be retried
// (misconfiguration). The row stays as failed evidence.
func (e *Engine) failRowPermanent(ctx context.Context, row qRow, msg string) error {
	_, err := e.db.ExecContext(ctx,
		`UPDATE download_queue
		    SET state = 'failed', attempts = attempts + 1,
		        next_attempt = '2999-12-31T23:59:59Z', last_error = ?
		  WHERE id = ?`, truncate(msg, 200), row.ID)
	e.log.Error("download failed permanently",
		"hash", row.Hash[:12], "variant", row.Variant, "error", msg)
	return err
}

// maybeActivate promotes the staging collection to active when it has
// no outstanding queue rows (all mandatory files verified — spec §8).
// The active tables (albums, photos) are rewritten in the SAME
// transaction as the collection state change: the renderer observes
// either the old collection or the new one, never a mix.
func (e *Engine) maybeActivate(ctx context.Context) {
	var rev int64
	err := e.db.QueryRowContext(ctx,
		`SELECT c.revision
		   FROM collections c
		  WHERE c.state = 'staging'
		    AND NOT EXISTS (SELECT 1 FROM download_queue dq WHERE dq.collection_revision = c.revision)`).
		Scan(&rev)
	if err != nil {
		return // no complete staging collection
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		e.log.Error("activation begin failed", "error", err)
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)

	// 1. Albums: replace the active set with the staged set.
	if err := replaceSet(ctx, tx, `albums`, `id`,
		`SELECT id, name, cover_hash, ? AS fetched_revision FROM staging_albums WHERE collection_revision = ?`,
		rev, rev); err != nil {
		e.log.Error("activation albums failed", "error", err)
		return
	}
	// 2. Photos: replace the active set with the staged set, writing
	//    the verified cache paths. Favourite state: local favourites
	//    win over the manifest when present (offline changes).
	if err := activatePhotos(ctx, tx, rev, e); err != nil {
		e.log.Error("activation photos failed", "error", err)
		return
	}
	// 3. Collection states.
	if _, err := tx.ExecContext(ctx,
		`UPDATE collections SET state = 'retired' WHERE state = 'active'`); err != nil {
		e.log.Error("activation retire failed", "error", err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE collections SET state = 'active', activated_at = ? WHERE revision = ? AND state = 'staging'`,
		now, rev); err != nil {
		e.log.Error("activation failed", "error", err)
		return
	}
	// Keep at most two retired collections (previous working
	// collections remain available; older ones are pruned so the table
	// stays bounded).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM collections
		  WHERE state = 'retired'
		    AND revision NOT IN (SELECT revision FROM collections
		                            WHERE state = 'retired'
		                            ORDER BY revision DESC LIMIT 2)`); err != nil {
		e.log.Error("retired prune failed", "error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		e.log.Error("activation commit failed", "error", err)
		return
	}

	e.setState(ctx, keyActiveRevision, fmt.Sprint(rev))
	e.setState(ctx, keyLastSuccess, now)
	e.resetFailures(ctx)
	e.gc(ctx)
	e.log.Info("collection activated", "revision", rev)
	e.fireActivate(rev)
}

// activatePhotos swaps the active photos table to the staged set.
func activatePhotos(ctx context.Context, tx *sql.Tx, rev int64, e *Engine) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT sp.hash, sp.album_id, sp.name, sp.caption, sp.capture_date,
		        sp.display_bytes, sp.thumb_bytes, sp.priority, sp.is_favourite,
		        sp.display_sha IS NOT NULL AND sp.display_sha != '',
		        sp.thumb_sha IS NOT NULL AND sp.thumb_sha != ''
		   FROM staging_photos sp WHERE sp.collection_revision = ?`, rev)
	if err != nil {
		return err
	}
	type actPhoto struct {
		hash, albumID, name, caption, capture string
		disB, thB                             int64
		prio, fav                             int
		haveDis, haveTh                       bool
	}
	var photos []actPhoto
	hashes := []string{}
	for rows.Next() {
		var p actPhoto
		var capDate sql.NullString
		if err := rows.Scan(&p.hash, &p.albumID, &p.name, &p.caption, &capDate,
			&p.disB, &p.thB, &p.prio, &p.fav, &p.haveDis, &p.haveTh); err != nil {
			rows.Close()
			return err
		}
		p.capture = capDate.String
		photos = append(photos, p)
		hashes = append(hashes, p.hash)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Local favourites win (offline-first conflict rule).
	localFav, err := localFavourites(ctx, tx)
	if err != nil {
		return err
	}

	for _, p := range photos {
		disPath, thPath := "", ""
		if p.haveDis {
			disPath = e.cache.Path(p.hash, cache.VariantDisplay)
		}
		if p.haveTh {
			thPath = e.cache.Path(p.hash, cache.VariantThumb)
		}
		fav := p.fav
		if v, ok := localFav[p.hash]; ok {
			fav = boolInt(v)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO photos (hash, album_id, name, caption, capture_date,
			                     display_bytes, thumb_bytes, display_path, thumb_path,
			                     priority, is_favourite, downloaded_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(hash) DO UPDATE SET
			   album_id = excluded.album_id, name = excluded.name,
			   caption = excluded.caption, capture_date = excluded.capture_date,
			   display_bytes = excluded.display_bytes, thumb_bytes = excluded.thumb_bytes,
			   display_path = excluded.display_path, thumb_path = excluded.thumb_path,
			   priority = excluded.priority, is_favourite = excluded.is_favourite`,
			p.hash, p.albumID, p.name, p.caption, nullIfEmpty(p.capture),
			p.disB, p.thB, disPath, nullIfEmpty(thPath), p.prio, fav,
			time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("activate photo %s: %w", p.hash, err)
		}
	}
	if err := deleteNotIn(ctx, tx, "photos", "hash", strSet(hashes)); err != nil {
		return err
	}
	return nil
}

func localFavourites(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT photo_hash, value FROM favourites")
	if err != nil {
		return nil, err
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

// replaceSet rewrites an active set table from a SELECT over the
// staging table (albums only in M3).
func replaceSet(ctx context.Context, tx *sql.Tx, table, idCol, selectStaged string, args ...any) error {
	rows, err := tx.QueryContext(ctx, selectStaged, args...)
	if err != nil {
		return err
	}
	type row struct {
		id    string
		name  string
		cover string
		rev   int64
	}
	var ids []string
	var out []row
	for rows.Next() {
		var r row
		var cover sql.NullString
		if err := rows.Scan(&r.id, &r.name, &cover, &r.rev); err != nil {
			rows.Close()
			return err
		}
		r.cover = cover.String
		ids = append(ids, r.id)
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range out {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO albums (id, name, cover_hash, fetched_revision) VALUES (?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
			   name = excluded.name, cover_hash = excluded.cover_hash,
			   fetched_revision = excluded.fetched_revision`,
			r.id, r.name, nullIfEmpty(r.cover), r.rev); err != nil {
			return err
		}
	}
	return deleteNotIn(ctx, tx, table, idCol, strSet(ids))
}

func strSet(ss []string) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		out[s] = true
	}
	return out
}

// gc removes cache files no photo row references (spec §8: GC must
// never remove active photographs). Newly committed files are
// referenced by their photo row as soon as commitRow runs, so they are
// kept; only truly orphaned files are removed.
func (e *Engine) gc(ctx context.Context) {
	rows, err := e.db.QueryContext(ctx,
		`SELECT display_path, thumb_path FROM photos`)
	if err != nil {
		return
	}
	defer rows.Close()
	keep := map[string]bool{}
	for rows.Next() {
		var dp, tp string
		if err := rows.Scan(&dp, &tp); err != nil {
			return
		}
		if dp != "" {
			keep[cacheKeyForPath(dp)] = true
		}
		if tp != "" {
			keep[cacheKeyForPath(tp)] = true
		}
	}
	if rows.Err() != nil {
		return
	}
	removed, err := e.cache.GC(func(h, v string) bool {
		return keep[h+"."+v]
	})
	if err != nil {
		e.log.Warn("gc failed", "error", err)
		return
	}
	if removed > 0 {
		e.log.Info("cache gc", "removed_files", removed)
	}
}

func cacheKeyForPath(p string) string {
	base := p
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '/' {
			base = base[i+1:]
			break
		}
	}
	return base
}

// ---------------------------------------------------------------------
// Policy (spec §9 "Download policies").

// policyAllowsNow reports whether the current policy permits
// non-priority downloads at this moment.
//   - immediate: always
//   - scheduled: only inside DownloadWindow
//   - hybrid: inside the window, or always when the pending batch is
//     small (<= HybridMaxBytes) — small changes need not wait.
func (e *Engine) policyAllowsNow() bool {
	switch e.syncCfg.Policy {
	case "", "immediate":
		return true
	case "scheduled":
		return inWindow(e.syncCfg.DownloadWindow)
	case "hybrid":
		if inWindow(e.syncCfg.DownloadWindow) {
			return true
		}
		total, _ := e.pendingBytes(context.Background())
		thr := e.syncCfg.HybridMaxBytes
		if thr <= 0 {
			thr = 20 << 20
		}
		return total <= thr
	default:
		return true
	}
}

// pendingBytes is the total size of files still queued for staging.
func (e *Engine) pendingBytes(ctx context.Context) (int64, error) {
	var n int64
	err := e.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(dq.bytes), 0)
		   FROM download_queue dq
		   JOIN collections c ON c.revision = dq.collection_revision
		  WHERE c.state = 'staging'`).Scan(&n)
	return n, err
}

// inWindow reports whether now is inside the daily window (fixed IANA
// zone). A window crossing midnight is supported (start > end).
func inWindow(w config.Window) bool {
	loc, err := time.LoadLocation(w.TZ)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	s, e := parseHM(w.Start), parseHM(w.End)
	cur := now.Hour()*60 + now.Minute()
	if s > e { // crosses midnight
		return cur >= s || cur < e
	}
	return cur >= s && cur < e
}

func parseHM(s string) int {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0
	}
	return h*60 + m
}

// ---------------------------------------------------------------------
// Scheduling helpers.

// randFloat64 draws from the engine's RNG under the shared mutex.
func (e *Engine) randFloat64() float64 {
	e.rngMu.Lock()
	defer e.rngMu.Unlock()
	return e.rng.Float64()
}

// jitter adds +/-maxFrac random jitter to a duration (may be negative;
// callers clamp).
func (e *Engine) jitter(d time.Duration, maxFrac float64) time.Duration {
	if maxFrac <= 0 {
		return 0
	}
	frac := e.randFloat64()*2 - 1 // [-1, 1]
	return time.Duration(float64(d) * maxFrac * frac)
}

// backoff returns base * 2^(attempt-1) capped at cap, with +-25%
// jitter. jitterFn supplies the random draw (mutex-guarded by the
// engine; tests pass a local source).
func backoff(attempt int, base, cap time.Duration, jitterFn func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	b := base
	for i := 1; i < attempt; i++ {
		b *= 2
		if b >= cap {
			b = cap
			break
		}
	}
	if b > cap {
		b = cap
	}
	frac := 0.75 + jitterFn()*0.5 // [0.75, 1.25]
	b = time.Duration(float64(b) * frac)
	if b > cap {
		b = cap
	}
	return b
}

// idle blocks until ctx is cancelled (unpaired engine).
func (e *Engine) idle(ctx context.Context) { <-ctx.Done() }

// ---------------------------------------------------------------------
// Small shared helpers.

func parseI64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func hashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sum := sha256.New()
	buf := make([]byte, 128<<10)
	if _, err := io.CopyBuffer(sum, f, buf); err != nil {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// deleteNotIn removes rows from table (keyed by col) whose key is not
// in the set. An empty set removes all rows.
func deleteNotIn(ctx context.Context, tx *sql.Tx, table, col string, set map[string]bool) error {
	if len(set) == 0 {
		_, err := tx.ExecContext(ctx, "DELETE FROM "+table)
		return err
	}
	ph := make([]string, 0, len(set))
	args := make([]any, 0, len(set))
	for k := range set {
		ph = append(ph, "?")
		args = append(args, k)
	}
	q := "DELETE FROM " + table + " WHERE " + col + " NOT IN (" + stringsJoin(ph, ",") + ")"
	_, err := tx.ExecContext(ctx, q, args...)
	return err
}

func stringsJoin(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
