package frame

// Milestone 5 frame-side end-to-end: the pairing protocol against a
// real management server (challenge → admin approval → one-time
// credential claim → sync), the persisted credential, and the
// effective-config precedence (config file < server < local).

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/frame/cache"
	"github.com/matthewgall/openframe/internal/frame/display"
	"github.com/matthewgall/openframe/internal/frame/pairing"
	"github.com/matthewgall/openframe/internal/frame/sync"
	"github.com/matthewgall/openframe/internal/id"
	"github.com/matthewgall/openframe/internal/logging"
	"github.com/matthewgall/openframe/internal/server"
	"github.com/matthewgall/openframe/internal/server/users"
	migrations "github.com/matthewgall/openframe/migrations"
)

// m5Server boots a real management server with an admin account.
func m5Server(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	ctx := context.Background()
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
	t.Cleanup(func() {
		ts.Close()
		sdb.Close()
	})
	if _, err := users.NewStore(sdb).Create(ctx, "admin@test", "Admin", "admin", "pw-admin-123"); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	return ts, &http.Client{Jar: jar}
}

func TestM5FramePairingFlow(t *testing.T) {
	ctx := context.Background()
	ts, httpc := m5Server(t)
	csrf := e2eLogin(t, httpc, ts.URL)

	// Frame side: identity + unpaired engine.
	frameDir := t.TempDir()
	fdb, err := db.Open(ctx, filepath.Join(frameDir, "frame.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, fdb, migrations.FS(), "frame"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fdb.Close() })
	identity, err := EnsureIdentity(ctx, fdb)
	if err != nil {
		t.Fatal(err)
	}
	flog, err := logging.New(logging.Options{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.New(filepath.Join(frameDir, "photos"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Frame.ServerURL = ts.URL
	eng, err := sync.New(identity, ts.URL, "", flog, fdb, c, &cfg.Frame)
	if err != nil {
		t.Fatal(err)
	}
	if eng.Paired() {
		t.Fatal("engine should start unpaired")
	}

	// 1. Request a challenge.
	ch, err := pairing.RequestChallenge(ctx, ts.URL, identity, "Test Frame")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if len(ch.Code) != 8 {
		t.Fatalf("code = %q", ch.Code)
	}

	// 2. The admin approves with the code the frame displayed.
	var approval struct {
		Credential string `json:"credential"`
	}
	e2ePostDecode(t, httpc, ts.URL, csrf, "POST",
		"/api/v1/pairing/"+ch.ID+"/approve", map[string]string{"code": ch.Code}, &approval)

	// 3. The frame polls and claims the credential — exactly once.
	st, err := pairing.PollStatus(ctx, ts.URL, ch.ID)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if st.State != pairing.StateApproved || st.DeviceID != identity {
		t.Fatalf("status = %+v", st)
	}
	if st.Credential != approval.Credential {
		t.Fatal("claimed credential differs from the approval response")
	}
	st2, err := pairing.PollStatus(ctx, ts.URL, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Credential != "" {
		t.Fatalf("credential claimed twice: %q", st2.Credential[:8])
	}

	// 4. Persist + reload the credential.
	credPath := filepath.Join(frameDir, credentialFileName)
	if err := SaveCredential(credPath, &Credential{
		DeviceID: identity, Token: st.Credential, PairedVia: "pairing",
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCredential(credPath)
	if err != nil || loaded == nil {
		t.Fatalf("load: (%v, %v)", loaded, err)
	}
	if loaded.Token != st.Credential || loaded.DeviceID != identity {
		t.Fatalf("roundtrip mismatch: %+v", loaded)
	}

	// 5. Install it and verify the full frame API now works.
	if err := eng.SetCredential(st.Credential); err != nil {
		t.Fatal(err)
	}
	if !eng.Paired() {
		t.Fatal("engine not paired after SetCredential")
	}
	payload := []byte(`{"version":"0.5.0","uptime_seconds":10,"last_sync_ok":true,
		"recent_errors":[],"update_state":"unknown"}`)
	seq, err := eng.ReportHealth(ctx, payload)
	if err != nil {
		t.Fatalf("health with claimed credential: %v", err)
	}
	if seq != 0 {
		t.Fatalf("initial sync seq = %d", seq)
	}

	// 6. resolveToken picks the persisted credential (no config token).
	f := &Frame{cfg: cfg, log: flog, db: fdb, identity: identity,
		errors: &errorRing{}, credPath: credPath, baseCfg: *cfg}
	if got := f.resolveToken(); got != st.Credential {
		t.Fatalf("resolveToken = %q", got[:8])
	}

	// A credential for a different identity is refused.
	if err := SaveCredential(credPath, &Credential{
		DeviceID: "99999999-9999-4999-8999-999999999999", Token: "ofdv1_x",
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.resolveToken(); got != "" {
		t.Fatalf("foreign credential accepted: %q", got[:8])
	}
}

// TestM5FramePairingRestartRecovery covers the lost-claim path: the
// admin approves, the frame loses power before claiming, and the next
// boot must recover via the 409 (device registered + challenge id)
// and claim the still-pending one-time credential.
func TestM5FramePairingRestartRecovery(t *testing.T) {
	ctx := context.Background()
	ts, httpc := m5Server(t)
	csrf := e2eLogin(t, httpc, ts.URL)

	frameDir := t.TempDir()
	fdb, err := db.Open(ctx, filepath.Join(frameDir, "frame.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, fdb, migrations.FS(), "frame"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fdb.Close() })
	identity, err := EnsureIdentity(ctx, fdb)
	if err != nil {
		t.Fatal(err)
	}

	// Boot 1: request a challenge, then lose power (no poll).
	ch, err := pairing.RequestChallenge(ctx, ts.URL, identity, "Recovery Frame")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	e2ePostDecode(t, httpc, ts.URL, csrf, "POST",
		"/api/v1/pairing/"+ch.ID+"/approve", map[string]string{"code": ch.Code}, nil)

	// Boot 2: the same identity asks for a challenge again.
	_, err = pairing.RequestChallenge(ctx, ts.URL, identity, "Recovery Frame")
	ar, ok := err.(*pairing.AlreadyRegistered)
	if !ok {
		t.Fatalf("want AlreadyRegistered, got %v", err)
	}
	if ar.ChallengeID != ch.ID {
		t.Fatalf("challenge id = %q, want %q", ar.ChallengeID, ch.ID)
	}

	// Polling that challenge still yields the one-time credential.
	st, err := pairing.PollStatus(ctx, ts.URL, ar.ChallengeID)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if st.State != pairing.StateApproved || st.Credential == "" {
		t.Fatalf("status = %+v", st)
	}

	// Claim, persist, pair.
	credPath := filepath.Join(frameDir, credentialFileName)
	if err := SaveCredential(credPath, &Credential{
		DeviceID: identity, Token: st.Credential, PairedVia: "pairing",
	}); err != nil {
		t.Fatal(err)
	}
	flog, err := logging.New(logging.Options{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.New(filepath.Join(frameDir, "photos"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Frame.ServerURL = ts.URL
	eng, err := sync.New(identity, ts.URL, "", flog, fdb, c, &cfg.Frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.SetCredential(st.Credential); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"version":"0.5.0","last_sync_ok":true,"recent_errors":[]}`)
	if _, err := eng.ReportHealth(ctx, payload); err != nil {
		t.Fatalf("health after recovery: %v", err)
	}
}

func TestM5EffectiveConfigPrecedence(t *testing.T) {
	ctx := context.Background()
	frameDir := t.TempDir()
	fdb, err := db.Open(ctx, filepath.Join(frameDir, "frame.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, fdb, migrations.FS(), "frame"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fdb.Close() })

	cfg := config.Default()
	// Distinct config-file values to prove the layering.
	cfg.Frame.Slideshow.Interval = 45 * time.Second
	cfg.Frame.Display.Brightness = 0.8
	cfg.Frame.Sleep.On = "23:00"
	cfg.Frame.Sleep.Off = "06:00"
	cfg.Frame.Display.Orientation = "0"

	flog, err := logging.New(logging.Options{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	drv, err := display.New(ctx, display.Options{Name: "headless", Width: 640, Height: 400, Log: flog})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drv.Close() })

	f := &Frame{cfg: cfg, log: flog, db: fdb, drv: drv, errors: &errorRing{}, baseCfg: *cfg}

	// No server config yet: the config file values hold.
	if err := f.recomputeEffective(ctx); err != nil {
		t.Fatal(err)
	}
	if f.cfg.Frame.Slideshow.Interval != 45*time.Second || f.cfg.Frame.Display.Brightness != 0.8 {
		t.Fatalf("baseline: %+v", f.cfg.Frame.Slideshow)
	}

	// Server config: overlays only its set fields.
	bri := 55
	f.applyServerConfig(sync.Config{
		Name:                 "Living Room",
		SlideshowIntervalSec: 90,
		Transition:           "crossfade",
		Brightness:           &bri,
		SleepOn:              "22:00",
		Orientation:          "rotate90",
	})
	if f.cfg.Frame.Slideshow.Interval != 90*time.Second {
		t.Fatalf("server interval = %v", f.cfg.Frame.Slideshow.Interval)
	}
	if f.cfg.Frame.Display.Brightness != 0.55 {
		t.Fatalf("server brightness = %v", f.cfg.Frame.Display.Brightness)
	}
	if f.cfg.Frame.Sleep.On != "22:00" || f.cfg.Frame.Sleep.Off != "06:00" {
		t.Fatalf("server sleep = %s/%s (off must survive)", f.cfg.Frame.Sleep.On, f.cfg.Frame.Sleep.Off)
	}
	if f.cfg.Frame.Display.Orientation != "90" {
		t.Fatalf("orientation mapping = %q", f.cfg.Frame.Display.Orientation)
	}

	// Local settings win over the server.
	if err := SaveLocalSetting(ctx, fdb, settingBrightness, "0.2"); err != nil {
		t.Fatal(err)
	}
	if err := f.recomputeEffective(ctx); err != nil {
		t.Fatal(err)
	}
	if f.cfg.Frame.Display.Brightness != 0.2 {
		t.Fatalf("local brightness lost: %v", f.cfg.Frame.Display.Brightness)
	}
	if f.cfg.Frame.Slideshow.Interval != 90*time.Second {
		t.Fatalf("server interval clobbered by recompute: %v", f.cfg.Frame.Slideshow.Interval)
	}

	// Clearing the local override falls back to the server value.
	if _, err := fdb.ExecContext(ctx,
		"DELETE FROM frame_settings WHERE key = ?", settingBrightness); err != nil {
		t.Fatal(err)
	}
	if err := f.recomputeEffective(ctx); err != nil {
		t.Fatal(err)
	}
	if f.cfg.Frame.Display.Brightness != 0.55 {
		t.Fatalf("fallback to server brightness failed: %v", f.cfg.Frame.Display.Brightness)
	}

	// The summary stored for `openframe status` is secret-free and
	// compact.
	summary, ok, err := getState(ctx, fdb, StateKeyServerConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("server config summary not stored")
	}
	if summary == "" {
		t.Fatal("server config summary not stored")
	}
	if strings.Contains(summary, "ofdv1") || strings.Contains(summary, "token") {
		t.Fatalf("summary leaks a secret: %q", summary)
	}
}

func TestM5BuildHealth(t *testing.T) {
	ctx := context.Background()
	frameDir := t.TempDir()
	fdb, err := db.Open(ctx, filepath.Join(frameDir, "frame.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, fdb, migrations.FS(), "frame"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fdb.Close() })

	identity, err := id.NewUUID()
	if err != nil {
		t.Fatal(err)
	}
	flog, err := logging.New(logging.Options{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	c, err := cache.New(filepath.Join(frameDir, "photos"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := sync.New(identity, "", "", flog, fdb, c, &cfg.Frame)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := display.New(ctx, display.Options{Name: "headless", Width: 320, Height: 200, Log: flog})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drv.Close() })

	f := &Frame{cfg: cfg, log: flog, db: fdb, drv: drv, cache: c, engine: eng,
		started: time.Now().Add(-time.Hour), errors: &errorRing{}, baseCfg: *cfg}
	f.RecordError("sync: server unreachable: connection refused")

	h := f.buildHealth(ctx)
	if h.Version == "" {
		t.Fatal("version empty")
	}
	if h.UptimeSeconds < 3500 {
		t.Fatalf("uptime = %d", h.UptimeSeconds)
	}
	if h.DisplayDriver != "headless" || h.DisplayStatus != "active" {
		t.Fatalf("display = %s/%s", h.DisplayDriver, h.DisplayStatus)
	}
	if h.ErrorCount24h != 1 || len(h.RecentErrors) != 1 ||
		!strings.HasPrefix(h.RecentErrors[0], "sync:") {
		t.Fatalf("errors = %+v", h)
	}
	if h.UpdateState != "unknown" {
		t.Fatalf("update state = %q", h.UpdateState)
	}
}
