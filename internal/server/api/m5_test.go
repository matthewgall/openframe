package api_test

// Milestone 5 end-to-end tests: the pairing protocol, health reporting,
// per-frame config distribution and remote sync requests (spec §6, §12,
// §13). These exercise the full HTTP surface the frame and the admin
// SPA use.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/matthewgall/openframe/internal/server/storage"
	migrations "github.com/matthewgall/openframe/migrations"
)

// m5Env is a running server + admin client. Unlike frameEnv it does
// NOT pre-create a device: the pairing tests create devices through
// the protocol itself.
type m5Env struct {
	t     *testing.T
	base  string
	admin *client
	dbh   *sql.DB
	frame *client // unauthenticated "frame" client
}

func newM5Env(t *testing.T) *m5Env {
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
	if _, err := storage.NewLocalFS(cfg.PhotoDir()); err != nil {
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
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	if code := admin.json(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": "admin@test", "password": "pw-admin-123"}, &out); code != http.StatusOK {
		t.Fatalf("login: %d", code)
	}
	admin.csrf = out.CSRFToken

	env := &m5Env{
		t:     t,
		base:  ts.URL,
		admin: admin,
		dbh:   dbh,
		frame: &client{t: t, base: ts.URL, http: &http.Client{}},
	}
	t.Cleanup(func() {
		ts.Close()
		dbh.Close()
	})
	return env
}

// frameRequest performs an unauthenticated frame-side request.
func (e *m5Env) frameRequest(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	resp, data := e.frame.do(method, path, body, false)
	var m map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &m)
	}
	return resp.StatusCode, m
}

// frameAuthed performs a device-credential request (Bearer).
func (e *m5Env) frameAuthed(deviceID, token string, method, path string, body []byte) (int, map[string]any) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.base+path, strings.NewReader(string(body)))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &m)
	}
	return resp.StatusCode, m
}

func TestM5PairingFlow(t *testing.T) {
	env := newM5Env(t)
	const fid = "11111111-2222-4333-8444-555555555555"

	// 1. The frame requests a challenge (public endpoint).
	code, out := env.frameRequest(http.MethodPost, "/api/v1/devices/pair-challenges", map[string]string{
		"device_id":   fid,
		"device_name": "Living Room",
	})
	if code != http.StatusCreated {
		t.Fatalf("challenge: %d %v", code, out)
	}
	challengeID, _ := out["challenge_id"].(string)
	pairCode, _ := out["code"].(string)
	if challengeID == "" || len(pairCode) != 8 {
		t.Fatalf("bad challenge response: %v", out)
	}

	// 2. The admin sees the pending request (without the code).
	var pending struct {
		Challenges []map[string]any `json:"challenges"`
	}
	resp, data := env.admin.do(http.MethodGet, "/api/v1/pairing/pending", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pending: %d", resp.StatusCode)
	}
	if err := json.Unmarshal(data, &pending); err != nil || len(pending.Challenges) != 1 {
		t.Fatalf("pending = %d entries: %s", len(pending.Challenges), data)
	}
	if _, has := pending.Challenges[0]["code"]; has {
		t.Fatal("pending list leaks the pairing code")
	}
	if name, _ := pending.Challenges[0]["device_name"].(string); name != "Living Room" {
		t.Fatalf("pending name = %q", name)
	}

	// 3. A wrong code is rejected; the challenge stays pending.
	resp, data = env.admin.do(http.MethodPost, "/api/v1/pairing/"+challengeID+"/approve",
		map[string]string{"code": "22222222"}, false)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong code: %d %s", resp.StatusCode, data)
	}

	// 4. The correct code approves: device + one-time credential.
	var approval struct {
		Device struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"device"`
		Credential string `json:"credential"`
	}
	resp, data = env.admin.do(http.MethodPost, "/api/v1/pairing/"+challengeID+"/approve",
		map[string]string{"code": pairCode}, false)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("approve: %d %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &approval); err != nil {
		t.Fatal(err)
	}
	if approval.Device.ID != fid {
		t.Fatalf("device id = %q, want the frame's identity %q", approval.Device.ID, fid)
	}
	if !strings.HasPrefix(approval.Credential, "ofdv1_") {
		t.Fatalf("credential format: %q", approval.Credential[:6])
	}

	// 5. The frame polls the status and claims the credential once.
	code, out = env.frameRequest(http.MethodPost, "/api/v1/devices/pair-status",
		map[string]string{"challenge_id": challengeID})
	if code != http.StatusOK || out["status"] != "approved" {
		t.Fatalf("status = %d %v", code, out)
	}
	claimed, _ := out["credential"].(string)
	if claimed != approval.Credential {
		t.Fatalf("claimed credential mismatch: %q", claimed)
	}
	code, out = env.frameRequest(http.MethodPost, "/api/v1/devices/pair-status",
		map[string]string{"challenge_id": challengeID})
	if c, _ := out["credential"].(string); c != "" {
		t.Fatalf("credential claimed twice: %q", c)
	}

	// 6. The claimed credential works on the frame API.
	code, out = env.frameAuthed(fid, claimed, http.MethodGet,
		"/api/v1/frames/"+fid+"/manifest", nil)
	if code != http.StatusOK || out["revision"] == nil {
		t.Fatalf("manifest with paired credential: %d %v", code, out)
	}

	// 7. The pending list is empty again; a new challenge for the same
	//    (now registered) identity is refused.
	resp, _ = env.admin.do(http.MethodGet, "/api/v1/pairing/pending", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	code, out = env.frameRequest(http.MethodPost, "/api/v1/devices/pair-challenges", map[string]string{
		"device_id": fid, "device_name": "Again",
	})
	if code != http.StatusConflict {
		t.Fatalf("re-pair registered device: %d %v", code, out)
	}

	// 8. Reject path on a second, fresh frame.
	const fid2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	code, out = env.frameRequest(http.MethodPost, "/api/v1/devices/pair-challenges", map[string]string{
		"device_id": fid2, "device_name": "Hall",
	})
	if code != http.StatusCreated {
		t.Fatalf("second challenge: %d", code)
	}
	secondID, _ := out["challenge_id"].(string)
	code, out = env.frameRequest(http.MethodPost, "/api/v1/devices/pair-status",
		map[string]string{"challenge_id": secondID})
	if out["status"] != "pending" {
		t.Fatalf("pre-reject status = %v", out["status"])
	}
	if code := env.admin.json(http.MethodPost, "/api/v1/pairing/"+secondID+"/reject", nil, nil); code != http.StatusOK {
		t.Fatalf("reject: %d", code)
	}
	code, out = env.frameRequest(http.MethodPost, "/api/v1/devices/pair-status",
		map[string]string{"challenge_id": secondID})
	if out["status"] != "rejected" {
		t.Fatalf("post-reject status = %v", out["status"])
	}
}

func TestM5PairingRateLimit(t *testing.T) {
	env := newM5Env(t)
	const ipPrefix = "10.9."
	// All test traffic arrives from the runner's loopback; the limit is
	// per source address, so a handful of challenges from one address
	// must be refused (flood control, spec §12).
	var last int
	for i := 0; i < 6; i++ {
		last, _ = env.frameRequest(http.MethodPost, "/api/v1/devices/pair-challenges", map[string]string{
			"device_id":   newFrameID(i),
			"device_name": "Flood " + ipPrefix,
		})
		if last == http.StatusTooManyRequests {
			break
		}
		if last != http.StatusCreated {
			t.Fatalf("challenge %d: %d", i, last)
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("rate limit never engaged; last = %d", last)
	}
}

// newFrameID returns a syntactically valid UUID (8-4-4-4-12), distinct per i.
func newFrameID(i int) string {
	return fmt.Sprintf("11111111-2222-4333-8444-5555555555%02d", i)
}

func TestM5HealthAndSyncRequest(t *testing.T) {
	env := newM5Env(t)
	const fid = "22222222-3333-4444-8555-666666666666"

	// Pair through the protocol (short path).
	_, out := env.frameRequest(http.MethodPost, "/api/v1/devices/pair-challenges", map[string]string{
		"device_id": fid, "device_name": "Health Frame",
	})
	challengeID, _ := out["challenge_id"].(string)
	pairCode, _ := out["code"].(string)
	if code := env.admin.json(http.MethodPost, "/api/v1/pairing/"+challengeID+"/approve",
		map[string]string{"code": pairCode}, nil); code != http.StatusCreated {
		t.Fatalf("approve: %d", code)
	}
	_, out = env.frameRequest(http.MethodPost, "/api/v1/devices/pair-status",
		map[string]string{"challenge_id": challengeID})
	token, _ := out["credential"].(string)
	if token == "" {
		t.Fatal("no credential claimed")
	}

	// 1. A healthy report → status online.
	payload := `{
		"version": "0.5.0", "uptime_seconds": 3600,
		"last_sync_at": "2026-11-12T10:00:00Z", "last_sync_ok": true,
		"manifest_revision": 3, "pending_downloads": 0,
		"storage_total": 100000000000, "storage_used": 40000000000, "cache_bytes": 30000000000,
		"mem_total_mb": 950, "mem_used_mb": 400, "cpu_temp_c": 48.5,
		"display_driver": "drm", "display_status": "active",
		"error_count_24h": 0, "recent_errors": [], "update_state": "unknown"
	}`
	code, out := env.frameAuthed(fid, token, http.MethodPost,
		"/api/v1/frames/"+fid+"/health", []byte(payload))
	if code != http.StatusOK {
		t.Fatalf("health: %d %v", code, out)
	}
	if seq, _ := out["sync_request_seq"].(float64); seq != 0 {
		t.Fatalf("initial seq = %v", seq)
	}

	var list struct {
		Devices []map[string]any `json:"devices"`
	}
	if code := env.admin.json(http.MethodGet, "/api/v1/devices", nil, &list); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if len(list.Devices) != 1 {
		t.Fatalf("devices = %d", len(list.Devices))
	}
	if st, _ := list.Devices[0]["status"].(string); st != "online" {
		t.Fatalf("status = %q, want online", st)
	}
	if _, ok := list.Devices[0]["health"].(map[string]any); !ok {
		t.Fatalf("health missing from list: %v", list.Devices[0])
	}

	// 2. A failing sync flips an online frame to sync_error.
	bad := strings.Replace(payload, `"last_sync_ok": true`, `"last_sync_ok": false`, 1)
	if code, _ := env.frameAuthed(fid, token, http.MethodPost,
		"/api/v1/frames/"+fid+"/health", []byte(bad)); code != http.StatusOK {
		t.Fatalf("health (bad sync): %d", code)
	}
	if code := env.admin.json(http.MethodGet, "/api/v1/devices", nil, &list); code != http.StatusOK {
		t.Fatal(code)
	}
	if st, _ := list.Devices[0]["status"].(string); st != "sync_error" {
		t.Fatalf("status = %q, want sync_error", st)
	}

	// 3. The detail endpoint exposes the report + config.
	var detail map[string]any
	if code := env.admin.json(http.MethodGet, "/api/v1/devices/"+fid, nil, &detail); code != http.StatusOK {
		t.Fatalf("detail: %d", code)
	}
	if detail["status"] != "sync_error" || detail["config"] == nil {
		t.Fatalf("detail = %v", detail)
	}

	// 4. Remote sync request: seq bumps and rides back on the health
	//    response (the frame's heartbeat — no inbound connection).
	var syncResp map[string]any
	if code := env.admin.json(http.MethodPost, "/api/v1/devices/"+fid+"/sync", nil, &syncResp); code != http.StatusOK {
		t.Fatalf("sync request: %d", code)
	}
	if seq, _ := syncResp["sync_request_seq"].(float64); seq != 1 {
		t.Fatalf("sync seq = %v", seq)
	}
	code, out = env.frameAuthed(fid, token, http.MethodPost,
		"/api/v1/frames/"+fid+"/health", []byte(payload))
	if seq, _ := out["sync_request_seq"].(float64); seq != 1 {
		t.Fatalf("heartbeat seq = %v, want 1", seq)
	}

	// 5. Cross-device scope: a token must not work against another
	//    device's path. The answer is an opaque 401 (it deliberately
	//    does not reveal whether the other device exists).
	code, _ = env.frameAuthed(fid, token, http.MethodPost,
		"/api/v1/frames/99999999-0000-4000-8000-000000000000/health", []byte(payload))
	if code != http.StatusUnauthorized {
		t.Fatalf("cross-device health: %d, want 401", code)
	}
}

func TestM5ConfigDistribution(t *testing.T) {
	env := newM5Env(t)
	const fid = "33333333-4444-4555-8666-777777777777"

	_, out := env.frameRequest(http.MethodPost, "/api/v1/devices/pair-challenges", map[string]string{
		"device_id": fid, "device_name": "Config Frame",
	})
	challengeID, _ := out["challenge_id"].(string)
	pairCode, _ := out["code"].(string)
	var approval struct {
		Credential string `json:"credential"`
	}
	if code := env.admin.json(http.MethodPost, "/api/v1/pairing/"+challengeID+"/approve",
		map[string]string{"code": pairCode}, &approval); code != http.StatusCreated {
		t.Fatalf("approve: %d", code)
	}
	token := approval.Credential

	// fetchManifest performs one unconditional manifest fetch.
	fetchManifest := func() (string, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, env.base+"/api/v1/frames/"+fid+"/manifest", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("manifest: %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("decode manifest: %v", err)
		}
		return resp.Header.Get("ETag"), m
	}

	// Baseline manifest: config present. A fresh device starts at
	// revision 1: the pairing set the device name, which is config
	// content.
	_, m := fetchManifest()
	cfg0, _ := m["config"].(map[string]any)
	if cfg0 == nil {
		t.Fatalf("manifest has no config: %v", m)
	}
	if name, _ := cfg0["name"].(string); name != "Config Frame" {
		t.Fatalf("config name = %q", name)
	}
	baseRev, _ := m["config_revision"].(float64)
	if baseRev != 1 {
		t.Fatalf("config_revision = %v, want 1", baseRev)
	}

	// PUT a valid config.
	cfgBody := map[string]any{
		"slideshow_interval_sec": 90,
		"transition":             "crossfade",
		"sleep_on":               "22:30",
		"sleep_off":              "07:15",
		"brightness":             55,
		"orientation":            "rotate90",
		"show_clock":             true,
		"sync_interval_sec":      120,
	}
	var cfgResp struct {
		Config struct {
			Version int64 `json:"version"`
		} `json:"config"`
	}
	if code := env.admin.json(http.MethodPut, "/api/v1/devices/"+fid+"/config", cfgBody, &cfgResp); code != http.StatusOK {
		t.Fatalf("put config: %d", code)
	}
	if cfgResp.Config.Version != 1 {
		t.Fatalf("config version = %d", cfgResp.Config.Version)
	}

	// The manifest now carries the config; config_revision bumped.
	_, m = fetchManifest()
	cfg1, _ := m["config"].(map[string]any)
	if cfg1["transition"] != "crossfade" || cfg1["slideshow_interval_sec"] != float64(90) ||
		cfg1["sleep_on"] != "22:30" || cfg1["brightness"] != float64(55) ||
		cfg1["orientation"] != "rotate90" || cfg1["show_clock"] != true {
		t.Fatalf("manifest config = %v", cfg1)
	}
	if cr, _ := m["config_revision"].(float64); cr != baseRev+1 {
		t.Fatalf("config_revision = %v, want %v", cr, baseRev+1)
	}

	// A config-only change must change the ETag (the frame re-fetches)
	// without bumping the content revision.
	req, _ := http.NewRequest(http.MethodGet, env.base+"/api/v1/frames/"+fid+"/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	etagBefore := resp.Header.Get("ETag")

	cfgBody["transition"] = "slide"
	if code := env.admin.json(http.MethodPut, "/api/v1/devices/"+fid+"/config", cfgBody, &cfgResp); code != http.StatusOK {
		t.Fatalf("put config 2: %d", code)
	}

	req, _ = http.NewRequest(http.MethodGet, env.base+"/api/v1/frames/"+fid+"/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	etagAfter := resp.Header.Get("ETag")
	if etagBefore == "" || etagBefore == etagAfter {
		t.Fatalf("config-only change did not change the ETag (%q → %q)", etagBefore, etagAfter)
	}
	var m2 map[string]any
	if err := json.Unmarshal(body, &m2); err != nil {
		t.Fatal(err)
	}
	// A config-only change must not bump the content revision.
	rev1, _ := m["revision"].(float64)
	rev2, _ := m2["revision"].(float64)
	if rev2 != rev1 {
		t.Fatalf("config change bumped content revision: %v → %v", rev1, rev2)
	}
	if cr, _ := m2["config_revision"].(float64); cr != baseRev+2 {
		t.Fatalf("config_revision = %v, want %v", cr, baseRev+2)
	}

	// If-None-Match with the old ETag must NOT 304 (config changed).
	req, _ = http.NewRequest(http.MethodGet, env.base+"/api/v1/frames/"+fid+"/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("If-None-Match", etagBefore)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stale-etag config fetch = %d, want 200", resp.StatusCode)
	}

	// Invalid documents are rejected and do not bump the version.
	bad := map[string]any{"transition": "dissolve"}
	if code := env.admin.json(http.MethodPut, "/api/v1/devices/"+fid+"/config", bad, nil); code != http.StatusBadRequest {
		t.Fatalf("bad config: %d, want 400", code)
	}
	bad = map[string]any{"sleep_on": "25:00"}
	if code := env.admin.json(http.MethodPut, "/api/v1/devices/"+fid+"/config", bad, nil); code != http.StatusBadRequest {
		t.Fatalf("bad sleep: %d, want 400", code)
	}
	var got struct {
		Config struct {
			Version int64 `json:"version"`
		} `json:"config"`
	}
	if code := env.admin.json(http.MethodGet, "/api/v1/devices/"+fid+"/config", nil, &got); code != http.StatusOK {
		t.Fatal(code)
	}
	if got.Config.Version != 2 {
		t.Fatalf("version = %d, want 2 (rejected writes must not bump)", got.Config.Version)
	}
}
