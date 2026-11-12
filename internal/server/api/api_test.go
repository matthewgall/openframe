package api_test // external test package: may import internal/server (no cycle)

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
	"github.com/matthewgall/openframe/internal/logging"
	"github.com/matthewgall/openframe/internal/server"
	"github.com/matthewgall/openframe/internal/server/users"
	"github.com/matthewgall/openframe/migrations"
)

// newTestServer builds the full server stack on a temp data dir and
// returns the test server plus the open DB handle (for seeding and
// assertions).
func newTestServer(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()

	serverDir, _, _ := cfg.Paths()
	dbh, err := db.Open(ctx, filepath.Join(serverDir, "server.db"), false)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log, err := logging.New(logging.Options{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, log, dbh)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(func() {
		ts.Close()
		dbh.Close()
	})
	return ts, dbh
}

type client struct {
	t    *testing.T
	http *http.Client
	base string
	csrf string
}

func (c *client) do(method, path string, body any, raw bool) (*http.Response, []byte) {
	c.t.Helper()
	var rdr io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(v)
	case []byte:
		rdr = bytes.NewReader(v)
	default:
		buf, _ := json.Marshal(v)
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		if _, ok := body.(string); ok {
			req.Header.Set("Content-Type", "text/plain")
		} else if !raw {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if c.csrf != "" && method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (c *client) json(method, path string, body, out any) int {
	c.t.Helper()
	resp, data := c.do(method, path, body, false)
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("decode %s %s: %v (body: %s)", method, path, err, data)
		}
	}
	return resp.StatusCode
}

// newClient logs in and returns an authenticated client with CSRF.
func newClient(t *testing.T, base, email, password string) *client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: base, http: &http.Client{Jar: jar}}
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	if code := c.json(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": email, "password": password}, &out); code != http.StatusOK {
		t.Fatalf("login %s: %d", email, code)
	}
	c.csrf = out.CSRFToken
	return c
}

// seedUser creates a user with a known plaintext password via the
// domain store (the production path is first-run bootstrap).
func seedUser(t *testing.T, dbh *sql.DB, email, role, password string) {
	t.Helper()
	u, err := users.NewStore(dbh).Create(context.Background(), email, email, role, password)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	_ = u
}

// makeJPEG renders a small gradient JPEG. Each call produces unique
// bytes (a varying seed channel) so content-hash deduplication does
// not merge separate test uploads.
var jpegSeed uint8

func makeJPEG(t *testing.T) []byte {
	t.Helper()
	jpegSeed++
	seed := jpegSeed
	img := image.NewNRGBA(image.Rect(0, 0, 1200, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1200; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x%256 + int(seed)), uint8(y % 256), 100, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// upload posts one file; returns (photoID, statusCode).
// When the server reports a per-file failure (e.g. HEIC), it returns
// ("", 0).
func (c *client) upload(t *testing.T, data []byte, name string) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("files", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, c.base+"/api/v1/photos/upload", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", c.csrf)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data2, _ := io.ReadAll(resp.Body)

	var out struct {
		Created []struct {
			Photo struct {
				ID string `json:"id"`
			} `json:"photo"`
		} `json:"created"`
		Failed []map[string]any `json:"failed"`
	}
	if err := json.Unmarshal(data2, &out); err != nil {
		t.Fatalf("decode upload response: %v (%s)", err, data2)
	}
	if len(out.Failed) > 0 {
		return "", 0
	}
	if len(out.Created) == 0 {
		return "", resp.StatusCode
	}
	return out.Created[0].Photo.ID, resp.StatusCode
}

func TestUnauthenticatedRejected(t *testing.T) {
	ts, _ := newTestServer(t)
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}}

	if code := c.json(http.MethodGet, "/api/v1/photos", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", code)
	}
	if code := c.json(http.MethodPost, "/api/v1/albums", map[string]string{"name": "X"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", code)
	}
	// Unknown API path -> JSON 404 (not the SPA fallback).
	resp, data := c.do(http.MethodGet, "/api/v1/nope", nil, false)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &errBody); err != nil || errBody.Error == "" {
		t.Fatalf("expected JSON error body, got: %s", data)
	}
}

func TestLoginRateLimiting(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "limited@example.com", "member", "right-pass-12345")

	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}}
	saw429 := false
	// The default per-IP limit is 5/min: the 6th+ attempt must be 429
	// even if the credential is right.
	for i := 0; i < 8; i++ {
		code := c.json(http.MethodPost, "/api/v1/auth/login",
			map[string]string{"email": "limited@example.com", "password": "wrong-pass-0000"}, nil)
		if code == http.StatusTooManyRequests {
			saw429 = true
			break
		}
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401/429, got %d", i+1, code)
		}
	}
	if !saw429 {
		t.Fatal("rate limiting never engaged after 8 failed logins")
	}
	// Per-account throttle: a correct password is now also 429.
	code := c.json(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": "limited@example.com", "password": "right-pass-12345"}, nil)
	if code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for throttled account, got %d", code)
	}
}

func TestFullFlow(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "admin@example.com", "admin", "admin-pass-12345")
	seedUser(t, dbh, "member@example.com", "member", "member-pass-12345")

	admin := newClient(t, ts.URL, "admin@example.com", "admin-pass-12345")
	member := newClient(t, ts.URL, "member@example.com", "member-pass-12345")

	// --- RBAC: admin can list users, member cannot. ---
	var usersOut struct {
		Users []map[string]any `json:"users"`
	}
	if code := admin.json(http.MethodGet, "/api/v1/users", nil, &usersOut); code != http.StatusOK {
		t.Fatalf("admin list users: %d", code)
	}
	if len(usersOut.Users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(usersOut.Users))
	}
	if code := member.json(http.MethodGet, "/api/v1/users", nil, nil); code != http.StatusForbidden {
		t.Fatalf("member list users: expected 403, got %d", code)
	}

	// --- Admin creates a user; temp password returned once. ---
	var created struct {
		User              map[string]any `json:"user"`
		TemporaryPassword string         `json:"temporary_password"`
	}
	code := admin.json(http.MethodPost, "/api/v1/users",
		map[string]string{"email": "new@example.com", "display_name": "New", "role": "member"}, &created)
	if code != http.StatusCreated {
		t.Fatalf("create user: %d", code)
	}
	if created.TemporaryPassword == "" {
		t.Fatal("no temporary password returned")
	}
	if code := admin.json(http.MethodPost, "/api/v1/users",
		map[string]string{"email": "new@example.com"}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate user: expected 409, got %d", code)
	}

	// --- Photo upload + dedupe. ---
	jpg := makeJPEG(t)
	photoID, code := admin.upload(t, jpg, "pic.jpg")
	if code != http.StatusCreated || photoID == "" {
		t.Fatalf("upload: %d %q", code, photoID)
	}
	dupID, _ := admin.upload(t, jpg, "pic-copy.jpg")
	if dupID != photoID {
		t.Fatalf("duplicate not deduplicated: %s vs %s", dupID, photoID)
	}

	// --- Derivatives: member gets thumb; original is uploader/admin only. ---
	resp, data := member.do(http.MethodGet, "/api/v1/photos/"+photoID+"/file?variant=thumb", nil, true)
	if resp.StatusCode != http.StatusOK || len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		t.Fatalf("thumb fetch: %d", resp.StatusCode)
	}
	resp, _ = member.do(http.MethodGet, "/api/v1/photos/"+photoID+"/file?variant=original", nil, true)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member original: expected 403, got %d", resp.StatusCode)
	}
	resp, data = admin.do(http.MethodGet, "/api/v1/photos/"+photoID+"/file?variant=original", nil, true)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(data, jpg) {
		t.Fatalf("admin original: %d", resp.StatusCode)
	}

	// --- HEIC rejected (per-file failure, no created entry). ---
	heic := append(append([]byte("\x00\x00\x00\x18ftyp"), []byte("heic")...), make([]byte, 8)...)
	if id, _ := member.upload(t, heic, "img.heic"); id != "" {
		t.Fatal("HEIC was accepted")
	}

	// --- Albums: create, add photo, cover, member visibility. ---
	var albumOut struct {
		Album map[string]any `json:"album"`
	}
	code = admin.json(http.MethodPost, "/api/v1/albums",
		map[string]any{"name": "Trips", "shared": true}, &albumOut)
	if code != http.StatusCreated {
		t.Fatalf("create album: %d", code)
	}
	albumID := albumOut.Album["id"].(string)
	if code := admin.json(http.MethodPost, "/api/v1/albums/"+albumID+"/photos",
		map[string]string{"photo_id": photoID}, nil); code != http.StatusOK {
		t.Fatalf("add photo to album: %d", code)
	}
	var albumGet struct {
		Album  map[string]any `json:"album"`
		Photos []string       `json:"photos"`
	}
	if code := member.json(http.MethodGet, "/api/v1/albums/"+albumID, nil, &albumGet); code != http.StatusOK {
		t.Fatalf("member view album: %d", code)
	}
	if len(albumGet.Photos) != 1 || albumGet.Album["cover_photo_id"] != photoID {
		t.Fatalf("album content: %+v", albumGet)
	}

	// --- Private album: non-creator member gets 403. ---
	var privateAlbum struct {
		Album map[string]any `json:"album"`
	}
	admin.json(http.MethodPost, "/api/v1/albums",
		map[string]any{"name": "Secret", "shared": false}, &privateAlbum)
	privateID := privateAlbum.Album["id"].(string)
	if code := member.json(http.MethodGet, "/api/v1/albums/"+privateID, nil, nil); code != http.StatusForbidden {
		t.Fatalf("private album: expected 403, got %d", code)
	}

	// --- Soft delete + recovery. ---
	if code := admin.json(http.MethodDelete, "/api/v1/photos/"+photoID, nil, nil); code != http.StatusOK {
		t.Fatalf("delete photo: %d", code)
	}
	if code := admin.json(http.MethodGet, "/api/v1/photos/"+photoID, nil, nil); code != http.StatusNotFound {
		t.Fatalf("deleted photo visible: %d", code)
	}
	var recovery struct {
		Photos []map[string]any `json:"photos"`
	}
	if code := admin.json(http.MethodGet, "/api/v1/photos?deleted=1", nil, &recovery); code != http.StatusOK || len(recovery.Photos) != 1 {
		t.Fatalf("recovery list: %d %d", code, len(recovery.Photos))
	}
	if code := admin.json(http.MethodPost, "/api/v1/photos/"+photoID+"/restore", nil, nil); code != http.StatusOK {
		t.Fatalf("restore photo: %d", code)
	}

	// --- Member can delete own upload but not others. ---
	memberPhoto, code := member.upload(t, makeJPEG(t), "mine.jpg")
	if code != http.StatusCreated {
		t.Fatalf("member upload: %d", code)
	}
	adminPhoto, _ := admin.upload(t, makeJPEG(t), "theirs.jpg")
	if code := member.json(http.MethodDelete, "/api/v1/photos/"+adminPhoto, nil, nil); code != http.StatusForbidden {
		t.Fatalf("member deleting other's photo: expected 403, got %d", code)
	}
	if code := member.json(http.MethodDelete, "/api/v1/photos/"+memberPhoto, nil, nil); code != http.StatusOK {
		t.Fatalf("member deleting own photo: %d", code)
	}

	// --- Stats + audit. ---
	var stats struct {
		Photos map[string]any `json:"photos"`
	}
	if code := admin.json(http.MethodGet, "/api/v1/stats", nil, &stats); code != http.StatusOK {
		t.Fatalf("stats: %d", code)
	}
	var auditOut struct {
		Entries []map[string]any `json:"entries"`
	}
	if code := admin.json(http.MethodGet, "/api/v1/audit", nil, &auditOut); code != http.StatusOK {
		t.Fatalf("audit: %d", code)
	}
	if len(auditOut.Entries) == 0 {
		t.Fatal("audit log empty after many actions")
	}
	if code := member.json(http.MethodGet, "/api/v1/audit", nil, nil); code != http.StatusForbidden {
		t.Fatalf("member audit: expected 403, got %d", code)
	}

	// --- Logout invalidates the session. ---
	if code := member.json(http.MethodPost, "/api/v1/auth/logout", nil, nil); code != http.StatusOK {
		t.Fatalf("logout: %d", code)
	}
	if code := member.json(http.MethodGet, "/api/v1/photos", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("post-logout request: expected 401, got %d", code)
	}
}

func TestChangePassword(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "a@example.com", "member", "old-pass-12345")
	c := newClient(t, ts.URL, "a@example.com", "old-pass-12345")

	// Wrong current password.
	if code := c.json(http.MethodPost, "/api/v1/auth/password",
		map[string]string{"current_password": "wrong-pass-99", "new_password": "new-pass-12345"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong current: expected 401, got %d", code)
	}
	// Success.
	if code := c.json(http.MethodPost, "/api/v1/auth/password",
		map[string]string{"current_password": "old-pass-12345", "new_password": "new-pass-12345"}, nil); code != http.StatusOK {
		t.Fatalf("change: %d", code)
	}
	// Old password no longer works; new one does.
	jar, _ := cookiejar.New(nil)
	c2 := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}}
	if code := c2.json(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": "a@example.com", "password": "old-pass-12345"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("old password still valid: %d", code)
	}
	if code := c2.json(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": "a@example.com", "password": "new-pass-12345"}, nil); code != http.StatusOK {
		t.Fatalf("new password rejected: %d", code)
	}
}
