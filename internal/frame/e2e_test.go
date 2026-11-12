package frame

// Milestone 4 end-to-end: a real management server and a real frame
// agent (headless driver + visual simulator) run the complete loop:
//
//	admin uploads + assigns -> frame syncs -> UI engine renders the
//	photo -> the headless simulator writes latest.png -> the test
//	decodes the PNG and verifies the photo's pixels are on screen.
//
// This exercises every M4 seam: sync activation -> UI reload,
// collection query -> canvas -> driver present -> snapshot.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/frame/display"
	"github.com/matthewgall/openframe/internal/frame/ui"
	"github.com/matthewgall/openframe/internal/logging"
	"github.com/matthewgall/openframe/internal/server"
	"github.com/matthewgall/openframe/internal/server/users"
	migrations "github.com/matthewgall/openframe/migrations"
)

func TestE2ETouchscreenDisplay(t *testing.T) {
	ctx := context.Background()
	const w, h = 640, 400

	// --- Server ------------------------------------------------------
	scfg := config.Default()
	scfg.DataDir = t.TempDir()
	serverDir, _, _ := scfg.Paths()
	sdb, err := db.Open(ctx, filepath.Join(serverDir, "server.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, sdb, migrations.FS(), "server"); err != nil {
		t.Fatal(err)
	}
	slogr, err := logging.New(logging.Options{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(scfg, slogr, sdb)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	if _, err := users.NewStore(sdb).Create(ctx, "admin@test", "Admin", "admin", "pw-admin-123"); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	httpc := &http.Client{Jar: jar}
	csrf := e2eLogin(t, httpc, ts.URL)
	deviceID := e2eCreateDevice(t, httpc, ts.URL, csrf)
	token := e2eIssueCredential(t, httpc, ts.URL, csrf, deviceID)
	albumID := e2eCreateAlbum(t, httpc, ts.URL, csrf)
	photoID := e2eUploadPhoto(t, httpc, ts.URL, csrf)
	e2ePost(t, httpc, ts.URL, csrf, "POST", "/api/v1/albums/"+albumID+"/photos", map[string]string{"photo_id": photoID})
	e2ePost(t, httpc, ts.URL, csrf, "POST", "/api/v1/albums/"+albumID+"/frames/"+deviceID, nil)

	// --- Frame ---------------------------------------------------------
	frameDir := t.TempDir()
	snapDir := filepath.Join(frameDir, "snapshots")
	fdb, err := db.Open(ctx, filepath.Join(frameDir, "frame.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, fdb, migrations.FS(), "frame"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fdb.Close() })

	// Pairing (M3 flow): the admin created the server device using
	// THIS frame's identity, and issued a credential for it. Seed the
	// identity so the frame's auto-generated ID matches the server
	// device (in production the operator reads the ID from
	// `openframe pair` and hands it to the admin).
	if _, err := fdb.ExecContext(ctx, "INSERT INTO identity (id, created_at) VALUES (?, ?)",
		deviceID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	drv, err := display.New(ctx, display.Options{
		Name:   "headless",
		Width:  w,
		Height: h,
		Log:    slogr,
		Snapshots: display.SnapshotConfig{
			Dir:      snapDir,
			Interval: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drv.Close() })

	fcfg := config.Default()
	fcfg.DataDir = frameDir
	fcfg.Frame.ServerURL = ts.URL
	fcfg.Frame.DeviceToken = token
	fcfg.Frame.Sync.Policy = "immediate"
	fcfg.Frame.Sync.ManifestInterval = 2 * time.Second
	fcfg.Frame.Sync.MaxConcurrent = 2
	// Keep the display on regardless of test run time.
	fcfg.Frame.Sleep.On = ""
	fcfg.Frame.Sleep.Off = ""

	flog, err := logging.New(logging.Options{Level: "warn"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := New(fcfg, flog, fdb, drv)
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Run(runCtx) }()

	// --- Wait until the UI has presented the uploaded photo -----------
	// The simulator writes latest.png on every present (rate-limited
	// to 1s here); poll until the centre pixel carries the photo's
	// red colour.
	latest := latestPNGPath(snapDir)
	deadline := time.Now().Add(60 * time.Second)
	var img *image.NRGBA
	for time.Now().Before(deadline) {
		if img = readLatestPNG(t, latest); img != nil {
			r, g, b, _ := img.At(w/2, h/3).RGBA()
			r8, g8, b8 := uint8(r>>8), uint8(g>>8), uint8(b>>8)
			if r8 > 150 && g8 < 100 && b8 < 100 {
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if img == nil {
		cancel()
		t.Fatalf("snapshot never appeared (UI never presented); frame run: %v", <-done)
	}
	r, g, b, _ := img.At(w/2, h/3).RGBA()
	if uint8(r>>8) <= 150 || uint8(g>>8) >= 100 || uint8(b>>8) >= 100 {
		cancel()
		<-done
		var active, queued int
		var lastSuccess, lastErr string
		fdb.QueryRow("SELECT COUNT(*) FROM photos").Scan(&active)
		fdb.QueryRow("SELECT COUNT(*) FROM download_queue").Scan(&queued)
		fdb.QueryRow("SELECT value FROM sync_state WHERE key='last_success'").Scan(&lastSuccess)
		fdb.QueryRow("SELECT value FROM sync_state WHERE key='last_error'").Scan(&lastErr)
		os.MkdirAll("/tmp/e2e", 0o755)
		os.WriteFile("/tmp/e2e/latest.png", mustRead(t, latestPNGPath(snapDir)), 0o644)
		t.Fatalf("photo never rendered: centre rgb(%d,%d,%d); screen=%v current=%v active_photos=%d queued=%d last_success=%q last_error=%q (snapshot /tmp/e2e/latest.png)",
			r>>8, g>>8, b>>8, f.ui.eng.Screen(), f.ui.eng.CurrentPhoto(), active, queued, lastSuccess, lastErr)
	}

	// The UI must be on the slideshow screen showing a photo.
	if f.ui == nil {
		t.Fatal("ui not built")
	}
	if got := f.ui.eng.Screen(); got != ui.ScreenSlideshow {
		t.Errorf("screen = %v, want slideshow", got)
	}
	if p := f.ui.eng.CurrentPhoto(); p == nil {
		t.Error("current photo is nil although the collection has one")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("frame Run returned error: %v", err)
	}
}

// readLatestPNG decodes the simulator snapshot if present.
func latestPNGPath(dir string) string { return filepath.Join(dir, "latest.png") }

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readLatestPNG(t *testing.T, path string) *image.NRGBA {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil
	}
	// png decodes to RGBA; convert for the caller.
	n := image.NewNRGBA(img.Bounds())
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			r, g, b, a := img.At(x, y).RGBA()
			n.SetNRGBA(x, y, color.NRGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)})
		}
	}
	return n
}

// --- helpers (self-contained; the M3 e2e helpers live in the sync
// --- package's test files) ---------------------------------------------

func e2eLogin(t *testing.T, c *http.Client, base string) string {
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
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.CSRFToken
}

func e2ePost(t *testing.T, c *http.Client, base, csrf, method, path string, body any) {
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

func e2ePostDecode(t *testing.T, c *http.Client, base, csrf, method, path string, body, out any) {
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
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

func e2eCreateDevice(t *testing.T, c *http.Client, base, csrf string) string {
	t.Helper()
	var out struct {
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
	}
	e2ePostDecode(t, c, base, csrf, "POST", "/api/v1/devices", map[string]string{"name": "E2E Frame"}, &out)
	if out.Device.ID == "" {
		t.Fatal("no device id")
	}
	return out.Device.ID
}

func e2eIssueCredential(t *testing.T, c *http.Client, base, csrf, deviceID string) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	e2ePostDecode(t, c, base, csrf, "POST", "/api/v1/devices/"+deviceID+"/credential", nil, &out)
	if out.Token == "" {
		t.Fatal("no credential token")
	}
	return out.Token
}

func e2eCreateAlbum(t *testing.T, c *http.Client, base, csrf string) string {
	t.Helper()
	var out struct {
		Album struct {
			ID string `json:"id"`
		} `json:"album"`
	}
	e2ePostDecode(t, c, base, csrf, "POST", "/api/v1/albums", map[string]string{"name": "E2E Album"}, &out)
	if out.Album.ID == "" {
		t.Fatal("no album id")
	}
	return out.Album.ID
}

func e2eUploadPhoto(t *testing.T, c *http.Client, base, csrf string) string {
	t.Helper()
	// A solid red JPEG: its centre must be recognisable after the
	// server re-encodes the display derivative.
	src := image.NewNRGBA(image.Rect(0, 0, 160, 120))
	for i := 0; i+3 < len(src.Pix); i += 4 {
		src.Pix[i], src.Pix[i+1], src.Pix[i+2], src.Pix[i+3] = 210, 30, 30, 255
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	fw, err := mw.CreateFormFile("files", "e2e-red.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", base+"/api/v1/photos/upload", &mp)
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
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Failed) > 0 {
		t.Fatalf("upload failed: %v", out.Failed)
	}
	if len(out.Created) == 0 {
		t.Fatal("upload created nothing")
	}
	return out.Created[0].Photo.ID
}

var _ = sql.ErrNoRows
