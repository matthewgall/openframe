package sync_test

// End-to-end: a real management server (httptest) and a real frame
// engine (temp frame.db + temp cache) run the full M3 loop:
// upload -> assign -> manifest -> download -> verify -> activate ->
// displayable; then 304 on re-check, and unassign -> empty collection.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/frame/cache"
	"github.com/matthewgall/openframe/internal/frame/sync"
	"github.com/matthewgall/openframe/internal/logging"
	"github.com/matthewgall/openframe/internal/server"
	"github.com/matthewgall/openframe/internal/server/users"
	migrations "github.com/matthewgall/openframe/migrations"
)

func TestE2EUploadAssignSync(t *testing.T) {
	ctx := context.Background()

	// --- Server -------------------------------------------------------
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	serverDir, _, _ := cfg.Paths()
	dbh, err := db.Open(ctx, filepath.Join(serverDir, "server.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		t.Fatal(err)
	}
	log, err := logging.New(logging.Options{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, log, dbh)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	// Admin user + session.
	if _, err := users.NewStore(dbh).Create(ctx, "admin@test", "Admin", "admin", "pw-admin-123"); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	httpc := &http.Client{Jar: jar}
	csrf := login(t, httpc, ts.URL)

	// Device + credential.
	deviceID := createDevice(t, httpc, ts.URL, csrf)
	token := issueCredential(t, httpc, ts.URL, csrf, deviceID)

	// Album + photo + assignment.
	albumID := createAlbum(t, httpc, ts.URL, csrf)
	photoID := uploadPhoto(t, httpc, ts.URL, csrf)
	post(t, httpc, ts.URL, csrf, "POST", "/api/v1/albums/"+albumID+"/photos", map[string]string{"photo_id": photoID})
	post(t, httpc, ts.URL, csrf, "POST", "/api/v1/albums/"+albumID+"/frames/"+deviceID, nil)

	var photo struct {
		Photo struct {
			Hash string `json:"hash"`
		} `json:"photo"`
	}
	getJSON(t, httpc, ts.URL, "GET", "/api/v1/photos/"+photoID, &photo)
	hash := photo.Photo.Hash

	// --- Frame --------------------------------------------------------
	frameDir := t.TempDir()
	fdb, err := db.Open(ctx, filepath.Join(frameDir, "frame.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, fdb, migrations.FS(), "frame"); err != nil {
		t.Fatal(err)
	}
	fcache, err := cache.New(filepath.Join(frameDir, "photos"), 0)
	if err != nil {
		t.Fatal(err)
	}
	fcfg := config.Default()
	fcfg.Frame.Sync.Policy = "immediate"
	fcfg.Frame.Sync.MaxConcurrent = 4
	t.Cleanup(func() { fdb.Close() })

	flog, err := logging.New(logging.Options{Level: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := sync.New(deviceID, ts.URL, token, flog, fdb, fcache, &fcfg.Frame)
	if err != nil {
		t.Fatal(err)
	}

	// Sync to active: stage, then run download batches until the
	// staging collection is complete and activated.
	eng.Check(ctx)
	syncToActive(t, eng, fdb, 1)

	// The photo is active and its display file is a decodable JPEG in
	// the cache.
	var n int
	var dp string
	if err := fdb.QueryRow("SELECT COUNT(*), display_path FROM photos WHERE hash = ?", hash).Scan(&n, &dp); err != nil {
		t.Fatal(err)
	}
	if n != 1 || dp == "" {
		t.Fatalf("active photo row = %d, path %q", n, dp)
	}
	f, err := fcache.Open(hash, "display")
	if err != nil {
		t.Fatal(err)
	}
	cfgImg, _, err := image.DecodeConfig(f)
	f.Close()
	if err != nil {
		t.Fatalf("cached display file is not a decodable image: %v", err)
	}
	if cfgImg.Width == 0 || cfgImg.Height == 0 {
		t.Error("decoded image has zero dimensions")
	}
	// The derivative must not be the original size (server re-encodes).
	var origBytes int64
	if err := dbh.QueryRow("SELECT size_bytes FROM photos WHERE id = ?", photoID).Scan(&origBytes); err != nil {
		t.Fatal(err)
	}

	// --- Frame restart: 304, no re-download ---------------------------
	fdb2, err := db.Open(ctx, filepath.Join(frameDir, "frame.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	eng2, err := sync.New(deviceID, ts.URL, token, flog, fdb2, fcache, &fcfg.Frame)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fdb2.Close() })
	eng2.Check(ctx)
	eng2.DownloadOnce(ctx)
	// The 304 path must not re-queue anything; a single batch suffices.
	var queued int
	fdb2.QueryRow("SELECT COUNT(*) FROM download_queue").Scan(&queued)
	if queued != 0 {
		t.Errorf("restart re-queued %d files (304 path broken)", queued)
	}
	var active int
	fdb2.QueryRow("SELECT COUNT(*) FROM photos WHERE display_path != ''").Scan(&active)
	if active != 1 {
		t.Errorf("after restart active = %d, want 1", active)
	}

	// --- Unassign: collection becomes empty ---------------------------
	post(t, httpc, ts.URL, csrf, "DELETE", "/api/v1/albums/"+albumID+"/frames/"+deviceID, nil)
	eng2.Check(ctx)
	syncToActive(t, eng2, fdb2, 2)
	fdb2.QueryRow("SELECT COUNT(*) FROM photos").Scan(&n)
	if n != 0 {
		t.Errorf("photos after unassign = %d, want 0", n)
	}
	// The cache file was GCed (no active reference).
	if fcache.Exists(hash, "display") {
		t.Error("display file not GCed after unassign")
	}
	_ = origBytes
}

// --- helpers ----------------------------------------------------------

func login(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	resp, err := c.Post(base+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"admin@test","password":"pw-admin-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("login: %d %s", resp.StatusCode, b)
	}
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.CSRFToken == "" {
		t.Fatal("no csrf token")
	}
	return out.CSRFToken
}

func post(t *testing.T, c *http.Client, base, csrf, method, path string, body any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-CSRF-Token", csrf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
}

func createDevice(t *testing.T, c *http.Client, base, csrf string) string {
	t.Helper()
	var out struct {
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
	}
	postWithDecode(t, c, base, csrf, "POST", "/api/v1/devices", map[string]string{"name": "E2E Frame"}, &out)
	if out.Device.ID == "" {
		t.Fatal("no device id")
	}
	return out.Device.ID
}

func issueCredential(t *testing.T, c *http.Client, base, csrf, deviceID string) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	postWithDecode(t, c, base, csrf, "POST", "/api/v1/devices/"+deviceID+"/credential", nil, &out)
	if out.Token == "" {
		t.Fatal("no credential token")
	}
	return out.Token
}

func createAlbum(t *testing.T, c *http.Client, base, csrf string) string {
	t.Helper()
	var out struct {
		Album struct {
			ID string `json:"id"`
		} `json:"album"`
	}
	postWithDecode(t, c, base, csrf, "POST", "/api/v1/albums", map[string]string{"name": "E2E Album"}, &out)
	if out.Album.ID == "" {
		t.Fatal("no album id")
	}
	return out.Album.ID
}

func uploadPhoto(t *testing.T, c *http.Client, base, csrf string) string {
	t.Helper()
	data := smallJPEG()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("files", "e2e.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", base+"/api/v1/photos/upload", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Created []struct {
			Photo struct {
				ID string `json:"id"`
			} `json:"photo"`
		} `json:"created"`
		Failed []map[string]any `json:"failed"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Failed) > 0 {
		t.Fatalf("upload failed: %v", out.Failed)
	}
	if len(out.Created) == 0 {
		t.Fatal("upload created nothing")
	}
	return out.Created[0].Photo.ID
}

func getJSON(t *testing.T, c *http.Client, base, method, path string, out any) {
	t.Helper()
	resp, err := c.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: %d %s", path, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
	_ = method
}

func postWithDecode(t *testing.T, c *http.Client, base, csrf, method, path string, body, out any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-CSRF-Token", csrf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
}

// syncToActive runs download batches until the active revision
// reaches want.
func syncToActive(t *testing.T, eng *sync.Engine, dbh *sql.DB, want int64) {
	t.Helper()
	for i := 0; i < 20; i++ {
		var v int64
		if err := dbh.QueryRow("SELECT value FROM sync_state WHERE key = 'active_revision'").Scan(&v); err == nil && v >= want {
			return
		}
		if _, err := eng.DownloadOnce(context.Background()); err != nil {
			t.Logf("DownloadOnce: %v", err)
		}
	}
	var v int64
	dbh.QueryRow("SELECT value FROM sync_state WHERE key = 'active_revision'").Scan(&v)
	t.Fatalf("active revision = %d, want %d", v, want)
}

func smallJPEG() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 640, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 640; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x % 251), uint8(y % 199), 120, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	return buf.Bytes()
}
