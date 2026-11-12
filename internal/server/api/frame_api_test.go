package api_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/logging"
	"github.com/matthewgall/openframe/internal/server"
	"github.com/matthewgall/openframe/internal/server/storage"
	migrations "github.com/matthewgall/openframe/migrations"
)

// frameEnv bundles a running server (with its storage root), an admin
// client and a registered device holding a credential.
type frameEnv struct {
	t      *testing.T
	base   string
	admin  *client
	dbh    *sql.DB
	store  storage.Storage
	device string
	token  string
}

func newFrameEnv(t *testing.T, withPhoto bool) *frameEnv {
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
	store, err := storage.NewLocalFS(cfg.PhotoDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
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
	seedUser(t, dbh, "admin@test", "admin", "pw-admin-123")
	jar, _ := cookiejar.New(nil)
	admin := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}}
	out := struct {
		CSRFToken string `json:"csrf_token"`
	}{}
	if code := admin.json(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": "admin@test", "password": "pw-admin-123"}, &out); code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}
	admin.csrf = out.CSRFToken

	env := &frameEnv{t: t, base: ts.URL, admin: admin, dbh: dbh, store: store}

	var dev struct {
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/devices", map[string]string{"name": "Test Frame"}, &dev); code != http.StatusCreated {
		t.Fatalf("create device: %d", code)
	}
	env.device = dev.Device.ID

	var cred struct {
		Token string `json:"token"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/devices/"+env.device+"/credential", nil, &cred); code != http.StatusCreated {
		t.Fatalf("issue credential: %d", code)
	}
	if !strings.HasPrefix(cred.Token, "ofdv1_") {
		t.Fatalf("credential token format: %q", cred.Token)
	}
	env.token = cred.Token

	t.Cleanup(func() {
		ts.Close()
		dbh.Close()
	})

	if withPhoto {
		env.addPhotoToAssignedAlbum()
	}
	return env
}

// addPhotoToAssignedAlbum creates an album, uploads one photo, adds
// it and assigns the album to the device. Returns the photo id.
func (e *frameEnv) addPhotoToAssignedAlbum() string {
	e.t.Helper()
	var album struct {
		Album struct {
			ID string `json:"id"`
		} `json:"album"`
	}
	if code := e.admin.json(http.MethodPost, "/api/v1/albums", map[string]string{"name": "Framed"}, &album); code != http.StatusCreated {
		e.t.Fatalf("create album: %d", code)
	}
	aid := album.Album.ID
	pid, code := e.admin.upload(e.t, makeJPEG(e.t), "photo.jpg")
	if code != http.StatusCreated || pid == "" {
		e.t.Fatalf("upload: %d %q", code, pid)
	}
	if code := e.admin.json(http.MethodPost, "/api/v1/albums/"+aid+"/photos", map[string]string{"photo_id": pid}, nil); code != http.StatusOK && code != http.StatusCreated {
		e.t.Fatalf("add photo to album: %d", code)
	}
	if code := e.admin.json(http.MethodPost, "/api/v1/albums/"+aid+"/frames/"+e.device, nil, nil); code != http.StatusOK && code != http.StatusCreated {
		e.t.Fatalf("assign device: %d", code)
	}
	return pid
}

func (e *frameEnv) getManifest(ifNoneMatch string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.base+"/api/v1/frames/"+e.device+"/manifest", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func (e *frameEnv) manifestJSON(body []byte) map[string]any {
	e.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		e.t.Fatalf("decode manifest: %v (%s)", err, body)
	}
	return m
}

func (e *frameEnv) download(resource, rangeHeader string) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.base+"/api/v1/frames/"+e.device+"/photos/"+resource, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// frameDo calls a frame endpoint with the device bearer token (the
// frame API is device-authenticated, never session-authenticated).
func (e *frameEnv) frameDo(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, e.base+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			e.t.Fatalf("decode %s: %v (%s)", path, err, data)
		}
	}
	return resp.StatusCode, out
}

// photoInManifest finds a photo entry (by content hash) anywhere in
// the manifest's albums.
func (e *frameEnv) photoInManifest(body []byte, hash string) map[string]any {
	e.t.Helper()
	m := e.manifestJSON(body)
	for _, a := range m["albums"].([]any) {
		for _, p := range a.(map[string]any)["photos"].([]any) {
			pm := p.(map[string]any)
			if pm["hash"] == hash {
				return pm
			}
		}
	}
	e.t.Fatalf("photo %s not found in manifest", hash)
	return nil
}

func TestFrameManifestETagAnd304(t *testing.T) {
	e := newFrameEnv(t, true)

	resp, body := e.getManifest("")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest: %d (%s)", resp.StatusCode, body)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("manifest missing ETag")
	}
	m := e.manifestJSON(body)
	if m["protocol"] != "openframe-manifest/1" {
		t.Errorf("protocol = %v", m["protocol"])
	}
	if rev, _ := m["revision"].(float64); rev != 1 {
		t.Errorf("revision = %v, want 1", m["revision"])
	}
	albums, _ := m["albums"].([]any)
	if len(albums) != 1 {
		t.Fatalf("albums = %d, want 1", len(albums))
	}
	photos := albums[0].(map[string]any)["photos"].([]any)
	if len(photos) != 1 {
		t.Fatalf("photos = %d, want 1", len(photos))
	}
	p := photos[0].(map[string]any)
	for _, variant := range []string{"display", "thumb"} {
		f, ok := p[variant].(map[string]any)
		if !ok {
			t.Fatalf("photo missing %s file", variant)
		}
		if f["sha256"] == "" || f["size_bytes"].(float64) <= 0 {
			t.Errorf("%s file missing sha256/size: %v", variant, f)
		}
	}

	// Conditional request: unchanged content -> 304, empty body.
	resp2, body2 := e.getManifest(etag)
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional: %d, want 304 (%s)", resp2.StatusCode, body2)
	}
	if len(body2) != 0 {
		t.Errorf("304 body = %d bytes, want 0", len(body2))
	}

	// New photo in the assigned album -> new revision + ETag.
	e.addPhotoToAssignedAlbum()
	resp3, body3 := e.getManifest(etag)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("after change: %d, want 200", resp3.StatusCode)
	}
	if resp3.Header.Get("ETag") == etag {
		t.Error("ETag unchanged after content change")
	}
	m3 := e.manifestJSON(body3)
	if rev3, _ := m3["revision"].(float64); rev3 != 2 {
		t.Errorf("revision = %v, want 2", m3["revision"])
	}
}

func TestFrameManifestConfigRevision(t *testing.T) {
	e := newFrameEnv(t, true)
	_, body := e.getManifest("")
	m := e.manifestJSON(body)
	manRev := m["revision"].(float64)
	cfgRev := m["config_revision"].(float64)

	// Renaming the device bumps the config revision but not the
	// manifest content revision (no photo re-downloads on a rename).
	var d struct {
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
	}
	if code := e.admin.json(http.MethodPatch, "/api/v1/devices/"+e.device, map[string]string{"name": "Renamed Frame"}, &d); code != http.StatusOK {
		t.Fatalf("rename: %d", code)
	}
	_, body2 := e.getManifest("")
	m2 := e.manifestJSON(body2)
	if m2["revision"].(float64) != manRev {
		t.Errorf("manifest revision changed on rename: %v -> %v", manRev, m2["revision"])
	}
	if m2["config_revision"].(float64) != cfgRev+1 {
		t.Errorf("config_revision = %v, want %v", m2["config_revision"], cfgRev+1)
	}
	if cfg := m2["config"].(map[string]any); cfg["name"] != "Renamed Frame" {
		t.Errorf("config.name = %v", cfg)
	}
}

func TestFrameDeviceAuth(t *testing.T) {
	e := newFrameEnv(t, true)

	// No token.
	e.token = ""
	resp, _ := e.getManifest("")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", resp.StatusCode)
	}

	// Wrong token.
	e.token = "ofdv1_" + strings.Repeat("A", 43)
	resp, _ = e.getManifest("")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: %d, want 401", resp.StatusCode)
	}

	// A different device's token must not work for this device id.
	e2 := newFrameEnv(t, false)
	e.token = e2.token
	resp, _ = e.getManifest("")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("other device's token: %d, want 401", resp.StatusCode)
	}
}

func TestFrameCredentialLifecycle(t *testing.T) {
	e := newFrameEnv(t, true)

	var list struct {
		Devices []struct {
			HasCredential bool `json:"has_credential"`
		} `json:"devices"`
	}
	if code := e.admin.json(http.MethodGet, "/api/v1/devices", nil, &list); code != http.StatusOK {
		t.Fatalf("list devices: %d", code)
	}
	found := false
	for _, d := range list.Devices {
		found = found || d.HasCredential
	}
	if !found {
		t.Error("device list does not report has_credential")
	}

	// Rotation invalidates the old token immediately.
	var cred struct {
		Token string `json:"token"`
	}
	if code := e.admin.json(http.MethodPost, "/api/v1/devices/"+e.device+"/credential", nil, &cred); code != http.StatusCreated {
		t.Fatalf("rotate: %d", code)
	}
	e.token = ""
	resp, _ := e.getManifest("")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old token after rotation: %d, want 401", resp.StatusCode)
	}
	e.token = cred.Token
	resp, _ = e.getManifest("")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("new token: %d, want 200", resp.StatusCode)
	}

	// Credential revocation blocks sync.
	if code := e.admin.json(http.MethodDelete, "/api/v1/devices/"+e.device+"/credential", nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke credential: %d", code)
	}
	resp, _ = e.getManifest("")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked credential: %d, want 401", resp.StatusCode)
	}

	// Revoked devices cannot be issued credentials.
	if code := e.admin.json(http.MethodPost, "/api/v1/devices/"+e.device+"/revoke", nil, nil); code != http.StatusOK {
		t.Fatalf("revoke device: %d", code)
	}
	code := e.admin.json(http.MethodPost, "/api/v1/devices/"+e.device+"/credential", nil, &cred)
	if code != http.StatusBadRequest {
		t.Errorf("issue to revoked device: %d, want 400", code)
	}
}

func TestFrameDownloadAuthorizationAndRange(t *testing.T) {
	e := newFrameEnv(t, true)
	pid := e.addPhotoToAssignedAlbum()

	var photo struct {
		Photo struct {
			Hash string `json:"hash"`
		} `json:"photo"`
	}
	if code := e.admin.json(http.MethodGet, "/api/v1/photos/"+pid, nil, &photo); code != http.StatusOK {
		t.Fatalf("get photo: %d", code)
	}
	hash := photo.Photo.Hash

	// Downloaded bytes must be byte-identical to the stored object
	// (independent read through the storage backend).
	resp, data := e.download(hash+".display", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download display: %d (%s)", resp.StatusCode, data)
	}
	rc, err := e.store.Open(context.Background(), storage.Key(hash+".display"))
	if err != nil {
		t.Fatalf("open stored object: %v", err)
	}
	want, _ := io.ReadAll(rc)
	rc.Close()
	if sha256Hex(data) != sha256Hex(want) {
		t.Error("downloaded bytes differ from the stored object")
	}
	if len(data) == 0 {
		t.Fatal("empty download")
	}

	// Suffix range: last 10 bytes.
	resp2, data2 := e.download(hash+".display", "bytes=-10")
	if resp2.StatusCode != http.StatusPartialContent {
		t.Fatalf("range: %d, want 206", resp2.StatusCode)
	}
	if len(data2) != 10 {
		t.Errorf("range length = %d, want 10", len(data2))
	}
	if string(data2) != string(data[len(data)-10:]) {
		t.Error("range bytes do not match the object tail")
	}
	cr := resp2.Header.Get("Content-Range")
	wantCR := "bytes " + strconv.Itoa(len(data)-10) + "-" + strconv.Itoa(len(data)-1) + "/" + strconv.Itoa(len(data))
	if cr != wantCR {
		t.Errorf("Content-Range = %q, want %q", cr, wantCR)
	}

	// Absolute range.
	resp2b, data2b := e.download(hash+".display", "bytes=0-9")
	if resp2b.StatusCode != http.StatusPartialContent || string(data2b) != string(data[:10]) {
		t.Errorf("absolute range: %d %q", resp2b.StatusCode, data2b[:min(10, len(data2b))])
	}

	// Unsatisfiable range.
	resp3, _ := e.download(hash+".display", "bytes=999999999-")
	if resp3.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("bad range: %d, want 416", resp3.StatusCode)
	}

	// A photo not assigned to the device is not downloadable.
	otherPid, _ := e.admin.upload(t, makeJPEG(t), "other.jpg")
	var other struct {
		Photo struct {
			Hash string `json:"hash"`
		} `json:"photo"`
	}
	if code := e.admin.json(http.MethodGet, "/api/v1/photos/"+otherPid, nil, &other); code != http.StatusOK {
		t.Fatalf("get other photo: %d", code)
	}
	resp4, _ := e.download(other.Photo.Hash+".display", "")
	if resp4.StatusCode != http.StatusNotFound {
		t.Errorf("unassigned photo download: %d, want 404", resp4.StatusCode)
	}

	// Unknown variant / malformed resource.
	resp5, _ := e.download(hash+".bogus", "")
	if resp5.StatusCode != http.StatusNotFound {
		t.Errorf("bad variant: %d, want 404", resp5.StatusCode)
	}
}

func TestFrameChangesIdempotency(t *testing.T) {
	e := newFrameEnv(t, true)
	pid := e.addPhotoToAssignedAlbum()
	var photo struct {
		Photo struct {
			Hash string `json:"hash"`
		} `json:"photo"`
	}
	e.admin.json(http.MethodGet, "/api/v1/photos/"+pid, nil, &photo)
	hash := photo.Photo.Hash

	post := func(ops []map[string]any) map[string]any {
		e.t.Helper()
		code, out := e.frameDo(http.MethodPost, "/api/v1/frames/"+e.device+"/changes",
			map[string]any{"ops": ops})
		if code != http.StatusOK {
			e.t.Fatalf("changes: %d", code)
		}
		return out
	}

	ops := []map[string]any{{
		"op_id": "op-1", "type": "favourite", "photo_hash": hash,
		"value": true, "ts": "2026-11-12T10:00:00Z",
	}}
	if out := post(ops); out["accepted"].(float64) != 1 {
		t.Errorf("first post: %v, want accepted=1", out)
	}

	// Exact replay: deduplicated, not re-applied.
	if out := post(ops); out["duplicates"].(float64) != 1 || out["accepted"].(float64) != 0 {
		t.Errorf("replay: %v, want duplicates=1 accepted=0", out)
	}

	// Malformed op (no ts): rejected and named.
	out := post([]map[string]any{{
		"op_id": "op-bad", "type": "favourite", "photo_hash": hash,
		"value": true,
	}})
	if out["rejected"].(float64) != 1 {
		t.Errorf("malformed: %v, want rejected=1", out)
	}
	if rej, _ := out["rejected_ops"].([]any); len(rej) != 1 || rej[0] != "op-bad" {
		t.Errorf("rejected_ops = %v, want [op-bad]", out["rejected_ops"])
	}

	// The favourite now appears in the manifest.
	resp, body := e.getManifest("")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest: %d", resp.StatusCode)
	}
	p := e.photoInManifest(body, hash)
	if p["favourite"] != true {
		t.Errorf("favourite not in manifest: %v", p)
	}

	// A newer unfavourite wins (last-write-wins).
	if out := post([]map[string]any{{
		"op_id": "op-2", "type": "favourite", "photo_hash": hash,
		"value": false, "ts": "2026-11-12T11:00:00Z",
	}}); out["accepted"].(float64) != 1 {
		t.Errorf("unfavourite: %v", out)
	}
	resp, body = e.getManifest("")
	p = e.photoInManifest(body, hash)
	// favourite=false is omitted from the JSON (omitempty): the key
	// must be absent or false.
	if v, ok := p["favourite"]; ok && v != false {
		t.Errorf("favourite after LWW: %v, want absent or false", v)
	}
}

func TestFramePriorityFlagInManifest(t *testing.T) {
	e := newFrameEnv(t, true)
	pid := e.addPhotoToAssignedAlbum()

	// The photo's hash (its album is the one the env created second).
	var photo struct {
		Photo struct {
			Hash string `json:"hash"`
		} `json:"photo"`
	}
	e.admin.json(http.MethodGet, "/api/v1/photos/"+pid, nil, &photo)
	hash := photo.Photo.Hash

	// Mark the photo priority; the manifest must reflect it.
	if code := e.admin.json(http.MethodPost, "/api/v1/photos/"+pid+"/priority", map[string]bool{"priority": true}, nil); code != http.StatusOK {
		t.Fatalf("set photo priority: %d", code)
	}
	resp, body := e.getManifest("")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest: %d", resp.StatusCode)
	}
	p := e.photoInManifest(body, hash)
	if p["priority"] != true {
		t.Errorf("photo priority not in manifest: %v", p)
	}

	// Unmark the photo, then use album priority: the photo must still
	// be priority-delivered (album flag propagates to its photos).
	if code := e.admin.json(http.MethodPost, "/api/v1/photos/"+pid+"/priority", map[string]bool{"priority": false}, nil); code != http.StatusOK {
		t.Fatalf("clear photo priority: %d", code)
	}
	resp, body = e.getManifest("")
	p = e.photoInManifest(body, hash)
	if p["priority"] == true {
		t.Errorf("photo still priority after unmark: %v", p)
	}

	var albums struct {
		Albums []struct {
			ID string `json:"id"`
		} `json:"albums"`
	}
	if code := e.admin.json(http.MethodGet, "/api/v1/albums", nil, &albums); code != http.StatusOK {
		t.Fatalf("list albums: %d", code)
	}
	// The photo's album is the last one (created most recently).
	aid := albums.Albums[len(albums.Albums)-1].ID
	if code := e.admin.json(http.MethodPost, "/api/v1/albums/"+aid+"/priority", map[string]bool{"priority": true}, nil); code != http.StatusOK {
		t.Fatalf("set album priority: %d", code)
	}
	resp, body = e.getManifest("")
	p = e.photoInManifest(body, hash)
	if p["priority"] != true {
		t.Errorf("album priority not propagated to photo: %v", p)
	}
}
