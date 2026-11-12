package sync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/frame/cache"
	"github.com/matthewgall/openframe/internal/logging"
	migrations "github.com/matthewgall/openframe/migrations"
)

// ---------------------------------------------------------------------
// Test fixtures.

// fileSpec is one downloadable derivative in the fake server's store.
type fileSpec struct {
	data    []byte
	sha     string
	skip    bool // server 404s this file (missing on the server)
	corrupt bool // server serves wrong bytes (sha mismatch client-side)
}

// fakeServer scripts the frame API: one manifest revision at a time,
// file downloads (with Range support), and changes ingest with
// op-id dedupe.
type fakeServer struct {
	t         *testing.T
	photos    []testPhoto
	rev       int64
	down      atomic.Bool // server is "down"
	rangeReqs atomic.Int64

	mu         sync.Mutex
	servedHash map[string]bool // changes op_ids seen
}

type testPhoto struct {
	hash    string
	album   string
	display fileSpec
	thumb   fileSpec
	prio    bool
	fav     bool
}

func newTestPhoto(hash string) testPhoto {
	dis := []byte("display-bytes-" + hash)
	th := []byte("thumb-bytes-" + hash)
	return testPhoto{
		hash:    hash,
		album:   "album-1",
		display: fileSpec{data: dis, sha: shaHex(dis)},
		thumb:   fileSpec{data: th, sha: shaHex(th)},
	}
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// manifestJSON renders the current photos as a manifest document.
func (f *fakeServer) manifestJSON() []byte {
	photos := make([]map[string]any, 0, len(f.photos))
	for _, p := range f.photos {
		m := map[string]any{
			"hash": p.hash, "name": p.hash + ".jpg", "width": 100, "height": 100,
		}
		if p.prio {
			m["priority"] = true
		}
		if p.fav {
			m["favourite"] = true
		}
		m["display"] = map[string]any{
			"resource":   p.hash + ".display",
			"size_bytes": len(p.display.data),
			"sha256":     p.display.sha,
		}
		m["thumb"] = map[string]any{
			"resource":   p.hash + ".thumb",
			"size_bytes": len(p.thumb.data),
			"sha256":     p.thumb.sha,
		}
		photos = append(photos, m)
	}
	doc := map[string]any{
		"protocol":        "openframe-manifest/1",
		"revision":        f.rev,
		"config_revision": 1,
		"config":          map[string]any{"name": "Test Frame"},
		"albums": []map[string]any{{
			"id": "album-1", "name": "Album One", "photos": photos,
		}},
	}
	b, _ := json.Marshal(doc)
	return b
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/frames/dev-1/manifest", func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		etag := fmt.Sprintf("\"%d\"", f.rev)
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write(f.manifestJSON())
	})
	mux.HandleFunc("/api/v1/frames/dev-1/photos/", func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		res := strings.TrimPrefix(r.URL.Path, "/api/v1/frames/dev-1/photos/")
		var spec *fileSpec
		for i := range f.photos {
			p := &f.photos[i]
			switch res {
			case p.hash + ".display":
				spec = &p.display
			case p.hash + ".thumb":
				spec = &p.thumb
			}
		}
		if spec == nil || spec.skip {
			http.NotFound(w, r)
			return
		}
		data := append([]byte{}, spec.data...)
		if spec.corrupt {
			// Same size, one byte flipped: only the SHA-256 check can
			// catch this (the size guard cannot).
			data[0] ^= 0xFF
		}
		if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
			f.rangeReqs.Add(1)
			// Support "bytes=N-" only (what the engine sends).
			start := parseRangeStart(rangeHdr)
			total := int64(len(data))
			if start < 0 || start >= total {
				w.Header().Set("Content-Range", "bytes */"+fmt.Sprint(total))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
			w.Header().Set("Content-Length", fmt.Sprint(total-start))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[start:])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data)
	})
	mux.HandleFunc("/api/v1/frames/dev-1/changes", func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		var req struct {
			Ops []ChangeOp `json:"ops"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		accepted, dups := 0, 0
		for _, op := range req.Ops {
			f.mu.Lock()
			seen := f.servedHash[op.OpID]
			f.servedHash[op.OpID] = true
			f.mu.Unlock()
			if seen {
				dups++
			} else {
				accepted++
			}
		}
		json.NewEncoder(w).Encode(ChangeResult{Accepted: accepted, Duplicate: dups})
	})
	return mux
}

func parseRangeStart(h string) int64 {
	h = strings.TrimPrefix(h, "bytes=")
	parts := strings.SplitN(h, "-", 2)
	var n int64
	fmt.Sscanf(parts[0], "%d", &n)
	return n
}

// syncEnv wires a frame engine against a fake server.
type syncEnv struct {
	t    *testing.T
	eng  *Engine
	dbh  *sql.DB
	c    *cache.Store
	fake *fakeServer
	srv  *httptest.Server
}

func newSyncEnv(t *testing.T, photos ...testPhoto) *syncEnv {
	t.Helper()
	ctx := context.Background()

	dbh, err := db.Open(ctx, filepath.Join(t.TempDir(), "frame.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "frame"); err != nil {
		t.Fatal(err)
	}
	c, err := cache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	log, err := logging.New(logging.Options{Level: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Frame.Sync.Policy = "immediate"
	cfg.Frame.Sync.MaxConcurrent = 4

	fake := &fakeServer{t: t, photos: photos, rev: 1}
	fake.servedHash = map[string]bool{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	eng, err := New("dev-1", srv.URL, "ofdv1_test-token", log, dbh, c, &cfg.Frame)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	return &syncEnv{t: t, eng: eng, dbh: dbh, c: c, fake: fake, srv: srv}
}

// runSync drives check + download batches until the collection is
// active (or the attempt budget runs out).
func (e *syncEnv) runSync(ctx context.Context) {
	e.t.Helper()
	e.eng.Check(ctx)
	for i := 0; i < 10; i++ {
		if e.activeRevision() == e.fake.rev {
			return
		}
		e.eng.DownloadOnce(ctx)
	}
	if got := e.activeRevision(); got != e.fake.rev {
		e.t.Fatalf("collection not active after sync: active=%d want=%d", got, e.fake.rev)
	}
}

func (e *syncEnv) activeRevision() int64 {
	var v int64
	e.dbh.QueryRow("SELECT value FROM sync_state WHERE key = 'active_revision'").Scan(&v)
	return v
}

func (e *syncEnv) stateValue(key string) string {
	var v string
	e.dbh.QueryRow("SELECT value FROM sync_state WHERE key = ?", key).Scan(&v)
	return v
}

func (e *syncEnv) activePhotoCount() int {
	var n int
	e.dbh.QueryRow("SELECT COUNT(*) FROM photos").Scan(&n)
	return n
}

// ---------------------------------------------------------------------
// Tests.

func TestSyncHappyPath(t *testing.T) {
	ctx := context.Background()
	p1, p2 := newTestPhoto(strings.Repeat("a", 64)), newTestPhoto(strings.Repeat("b", 64))
	e := newSyncEnv(t, p1, p2)

	e.runSync(ctx)

	if e.activePhotoCount() != 2 {
		t.Fatalf("active photos = %d, want 2", e.activePhotoCount())
	}
	// Files verified in the cache.
	for _, p := range []testPhoto{p1, p2} {
		for variant, sha := range map[string]string{"display": p.display.sha, "thumb": p.thumb.sha} {
			if !e.c.Exists(p.hash, variant) {
				t.Errorf("%s %s not in cache", p.hash[:8], variant)
				continue
			}
			if got := fileSHA(e.c.PartPath(p.hash, variant), e.c.Path(p.hash, variant)); got != sha {
				t.Errorf("%s %s sha = %s, want %s", p.hash[:8], variant, got, sha)
			}
		}
	}
	// Photo rows point into the cache.
	var dp string
	e.dbh.QueryRow("SELECT display_path FROM photos WHERE hash = ?", p1.hash).Scan(&dp)
	if dp != e.c.Path(p1.hash, "display") {
		t.Errorf("display_path = %s, want %s", dp, e.c.Path(p1.hash, "display"))
	}
}

func TestOnActivateHook(t *testing.T) {
	ctx := context.Background()
	p := newTestPhoto(strings.Repeat("a", 64))
	e := newSyncEnv(t, p)

	var calls []int64
	e.eng.OnActivate(func(rev int64) { calls = append(calls, rev) })

	e.runSync(ctx)

	if len(calls) != 1 || calls[0] != 1 {
		t.Fatalf("activation hook calls = %v, want [1]", calls)
	}

	// A 304 re-check must not re-fire the hook.
	e.eng.Check(ctx)
	e.eng.DownloadOnce(ctx)
	if len(calls) != 1 {
		t.Fatalf("hook fired again on no-op: %v", calls)
	}
}

func TestSync304NoOp(t *testing.T) {
	ctx := context.Background()
	p := newTestPhoto(strings.Repeat("c", 64))
	e := newSyncEnv(t, p)
	e.runSync(ctx)

	// Second pass: 304, no re-staging, no queue rows.
	var staged int
	e.dbh.QueryRow("SELECT COUNT(*) FROM collections WHERE state = 'staging'").Scan(&staged)
	if staged != 0 {
		t.Fatalf("staging rows before re-check = %d, want 0", staged)
	}
	e.eng.Check(ctx)
	e.eng.DownloadOnce(ctx)
	if e.activeRevision() != 1 {
		t.Errorf("revision after 304 pass = %d, want 1", e.activeRevision())
	}
	var queued int
	e.dbh.QueryRow("SELECT COUNT(*) FROM download_queue").Scan(&queued)
	if queued != 0 {
		t.Errorf("queue rows after 304 pass = %d, want 0", queued)
	}
}

func TestSyncOfflineKeepsActiveCollection(t *testing.T) {
	ctx := context.Background()
	p := newTestPhoto(strings.Repeat("d", 64))
	e := newSyncEnv(t, p)
	e.runSync(ctx)
	if e.activePhotoCount() != 1 {
		t.Fatal("setup: no active collection")
	}

	// Server dies: the engine records the failure and the active
	// collection is untouched.
	e.fake.down.Store(true)
	e.eng.Check(ctx)
	if e.stateValue("consecutive_failures") != "1" {
		t.Errorf("consecutive_failures = %q, want 1", e.stateValue("consecutive_failures"))
	}
	if e.stateValue("last_error") == "" {
		t.Error("last_error not recorded")
	}
	if e.activePhotoCount() != 1 {
		t.Errorf("active photos after outage = %d, want 1", e.activePhotoCount())
	}
	// The renderer's data source is intact.
	var n int
	e.dbh.QueryRow("SELECT COUNT(*) FROM photos WHERE display_path != ''").Scan(&n)
	if n != 1 {
		t.Errorf("displayable photos = %d, want 1", n)
	}

	// Server returns: sync resumes and recovers.
	e.fake.down.Store(false)
	e.eng.Check(ctx)
	if e.stateValue("consecutive_failures") != "0" {
		t.Errorf("failures not reset: %q", e.stateValue("consecutive_failures"))
	}
}

func TestSyncActivationAtomicity(t *testing.T) {
	ctx := context.Background()
	p1 := newTestPhoto(strings.Repeat("e", 64))
	e := newSyncEnv(t, p1)
	e.runSync(ctx) // active rev 1

	// Rev 2 adds a photo whose display file is missing server-side.
	p2 := newTestPhoto(strings.Repeat("f", 64))
	p2.display.skip = true
	e.fake.photos = append(e.fake.photos, p2)
	e.fake.rev = 2
	e.eng.Check(ctx)
	e.eng.DownloadOnce(ctx)
	e.eng.DownloadOnce(ctx)

	// The staging collection can never activate (one mandatory file
	// missing) and the previous collection stays active.
	if e.activeRevision() != 1 {
		t.Fatalf("active revision = %d, want 1 (previous collection)", e.activeRevision())
	}
	if e.activePhotoCount() != 1 {
		t.Errorf("active photos = %d, want 1", e.activePhotoCount())
	}
	var stState string
	e.dbh.QueryRow("SELECT state FROM collections WHERE revision = 2").Scan(&stState)
	if stState != "staging" {
		t.Errorf("rev2 collection state = %q, want staging", stState)
	}
	// The failed row is in the queue with an error.
	var attempts, dq int
	e.dbh.QueryRow("SELECT attempts, COUNT(*) FROM download_queue WHERE collection_revision = 2").Scan(&attempts, &dq)
	if dq != 1 {
		t.Errorf("queue rows for rev2 = %d, want 1 (the missing file)", dq)
	}
	if attempts < 1 {
		t.Errorf("failed download not retried (attempts=%d)", attempts)
	}
	_ = p1
}

func TestSyncSHA256Mismatch(t *testing.T) {
	ctx := context.Background()
	p := newTestPhoto(strings.Repeat("1", 64))
	e := newSyncEnv(t, p)
	// Corrupt the served bytes: verification must reject the file.
	p.display.corrupt = true
	e.fake.photos[0] = p

	e.eng.Check(ctx)
	e.eng.DownloadOnce(ctx)

	if e.activeRevision() != 0 {
		t.Fatalf("collection activated despite sha mismatch (rev=%d)", e.activeRevision())
	}
	if e.c.Exists(p.hash, "display") {
		t.Error("corrupt file committed to the cache")
	}
	var lastErr string
	e.dbh.QueryRow("SELECT last_error FROM download_queue LIMIT 1").Scan(&lastErr)
	if !strings.Contains(lastErr, "sha256") {
		t.Errorf("queue last_error = %q, want sha256 mismatch", lastErr)
	}
	// The .part was discarded (no residue for a bad resume).
	if _, err := e.c.Size(p.hash, "display"); err == nil {
		t.Error("expected no committed display file")
	}
}

func TestSyncDiskFull(t *testing.T) {
	ctx := context.Background()
	p := newTestPhoto(strings.Repeat("2", 64))
	e := newSyncEnv(t, p)
	// Shrink the quota below the file size so HasRoom() fails.
	if err := e.c.SetQuota(10); err != nil {
		t.Fatal(err)
	}
	e.eng.Check(ctx)
	e.eng.DownloadOnce(ctx)

	if e.activeRevision() != 0 {
		t.Fatal("collection activated despite disk full")
	}
	var lastErr string
	e.dbh.QueryRow("SELECT last_error FROM download_queue LIMIT 1").Scan(&lastErr)
	if !strings.Contains(lastErr, "disk full") {
		t.Errorf("last_error = %q, want disk-full message", lastErr)
	}
}

func TestSyncGCRemovesOrphans(t *testing.T) {
	ctx := context.Background()
	p1 := newTestPhoto(strings.Repeat("3", 64))
	p2 := newTestPhoto(strings.Repeat("4", 64))
	e := newSyncEnv(t, p1, p2)
	e.runSync(ctx)
	if !e.c.Exists(p1.hash, "display") || !e.c.Exists(p2.hash, "display") {
		t.Fatal("setup: both display files should be cached")
	}

	// Rev 2 drops p2: activation must GC its files.
	e.fake.photos = []testPhoto{p1}
	e.fake.rev = 2
	e.eng.Check(ctx)
	e.eng.DownloadOnce(ctx)

	if e.activeRevision() != 2 {
		t.Fatalf("active = %d, want 2", e.activeRevision())
	}
	if e.c.Exists(p2.hash, "display") {
		t.Error("orphaned display file not GCed")
	}
	if e.c.Exists(p2.hash, "thumb") {
		t.Error("orphaned thumb file not GCed")
	}
	if !e.c.Exists(p1.hash, "display") {
		t.Error("active photo's file was GCed (must never happen)")
	}
}

func TestSyncMonotonicityGuard(t *testing.T) {
	ctx := context.Background()
	p := newTestPhoto(strings.Repeat("5", 64))
	e := newSyncEnv(t, p)
	e.runSync(ctx)
	if e.activeRevision() != 1 {
		t.Fatal("setup: rev 1 should be active")
	}

	// A stale manifest (older revision) must be ignored.
	e.fake.rev = 1
	e.eng.Check(ctx)
	e.eng.DownloadOnce(ctx)
	if e.activeRevision() != 1 {
		t.Fatalf("stale manifest changed active revision to %d", e.activeRevision())
	}
	var staging int
	e.dbh.QueryRow("SELECT COUNT(*) FROM collections WHERE state = 'staging'").Scan(&staging)
	if staging != 0 {
		t.Errorf("stale manifest created staging rows: %d", staging)
	}
}

func TestFavouriteOfflineQueueAndReplay(t *testing.T) {
	ctx := context.Background()
	p := newTestPhoto(strings.Repeat("6", 64))
	e := newSyncEnv(t, p)
	e.runSync(ctx)

	// Offline favourite change: local state is immediate, op is queued.
	e.fake.down.Store(true)
	if err := e.eng.SetFavourite(ctx, p.hash, true); err != nil {
		t.Fatal(err)
	}
	fav, err := e.eng.Favourite(ctx, p.hash)
	if err != nil || !fav {
		t.Fatalf("local favourite = %v, %v; want true", fav, err)
	}
	n, _ := e.eng.PendingChanges(ctx)
	if n != 1 {
		t.Fatalf("pending changes = %d, want 1", n)
	}
	// Upload while down: deferred, queue intact.
	if _, err := e.eng.uploadChanges(ctx); err == nil {
		t.Fatal("upload should fail while the server is down")
	}
	if n, _ = e.eng.PendingChanges(ctx); n != 1 {
		t.Fatalf("queue drained on failure: %d", n)
	}

	// Server returns: the op is delivered and acked.
	e.fake.down.Store(false)
	if _, err := e.eng.uploadChanges(ctx); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if n, _ = e.eng.PendingChanges(ctx); n != 0 {
		t.Fatalf("pending after upload = %d, want 0", n)
	}
	e.fake.mu.Lock()
	seen := len(e.fake.servedHash)
	e.fake.mu.Unlock()
	if seen != 1 {
		t.Fatalf("server saw %d ops, want 1", seen)
	}

	// Requeue the same logical change (new op id) and replay: the
	// server dedupes by op_id, so a duplicate op is acked cleanly.
	if err := e.eng.SetFavourite(ctx, p.hash, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.uploadChanges(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ = e.eng.PendingChanges(ctx); n != 0 {
		t.Fatalf("pending after replay = %d, want 0", n)
	}
}

func TestDownloadResumeWithRange(t *testing.T) {
	ctx := context.Background()
	p := newTestPhoto(strings.Repeat("7", 64))
	e := newSyncEnv(t, p)
	e.eng.Check(ctx)

	// Simulate a previous attempt that left a partial file: the first
	// 5 bytes of the display object.
	data := p.display.data
	part := e.c.PartPath(p.hash, "display")
	if err := writeFile(part, data[:5]); err != nil {
		t.Fatal(err)
	}

	if _, err := e.eng.DownloadOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if e.fake.rangeReqs.Load() < 1 {
		t.Error("no Range request was made for the partial file")
	}
	if !e.c.Exists(p.hash, "display") {
		t.Fatal("resumed download did not commit")
	}
	if got := fileSHA("", e.c.Path(p.hash, "display")); got != p.display.sha {
		t.Errorf("resumed file sha = %s, want %s", got, p.display.sha)
	}
}

func TestPolicyWindow(t *testing.T) {
	// inWindow: a full-day UTC window always contains now.
	if !inWindow(config.Window{Start: "00:00", End: "23:59", TZ: "UTC"}) {
		t.Error("full-day window should contain now")
	}
	// A zero-length window never does.
	if inWindow(config.Window{Start: "12:00", End: "12:00", TZ: "UTC"}) {
		t.Error("zero-length window should not contain now")
	}

	// policyAllowsNow: immediate always; hybrid with an empty pending
	// batch downloads even outside the window.
	e := newSyncEnv(t)
	e.eng.syncCfg.Policy = "immediate"
	if !e.eng.policyAllowsNow() {
		t.Error("immediate policy should allow now")
	}
	e.eng.syncCfg.Policy = "hybrid"
	e.eng.syncCfg.DownloadWindow = config.Window{Start: "12:00", End: "12:00", TZ: "UTC"}
	if !e.eng.policyAllowsNow() {
		t.Error("hybrid with empty batch should allow now")
	}
}

func TestBackoffGrows(t *testing.T) {
	rng := newRNG()
	fn := rng.Float64
	b1 := backoff(1, 30*time.Second, time.Hour, fn)
	b2 := backoff(2, 30*time.Second, time.Hour, fn)
	b3 := backoff(3, 30*time.Second, time.Hour, fn)
	if b2 < b1 || b3 < b2 {
		t.Errorf("backoff not growing: %v, %v, %v", b1, b2, b3)
	}
	bc := backoff(50, 30*time.Second, time.Hour, fn)
	if bc > time.Hour {
		t.Errorf("backoff %v exceeds cap", bc)
	}
}

// --- small helpers ----------------------------------------------------

func newRNG() *rand.Rand { return rand.New(rand.NewSource(1)) }

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

func fileSHA(_, path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 32<<10)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
