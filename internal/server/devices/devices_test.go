package devices

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/id"
	"github.com/matthewgall/openframe/migrations"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbh, err := db.Open(ctx, t.TempDir()+"/test.db", false)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	return dbh
}

func newDevice(t *testing.T, s *Store, name string) string {
	t.Helper()
	d, err := s.Create(context.Background(), name)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	return d.ID
}

func TestHealthReportAndStatus(t *testing.T) {
	ctx := context.Background()
	dbh := newTestDB(t)
	s := NewStore(dbh)
	did := newDevice(t, s, "F1")

	h := &HealthReport{
		Version: "0.5.0", UptimeSeconds: 3600, LastSyncAt: "2026-11-12T10:00:00Z",
		LastSyncOK: true, ManifestRevision: 7, PendingDownloads: 2,
		StorageTotal: 1000, StorageUsed: 400, CacheBytes: 300,
		MemTotalMB: 950, MemUsedMB: 400, DisplayDriver: "drm",
		DisplayStatus: "active", RecentErrors: []string{"sync: timeout"},
		UpdateState: "unknown",
	}
	if err := s.ReportHealth(ctx, did, h); err != nil {
		t.Fatalf("report: %v", err)
	}

	got, err := s.GetHealth(ctx, did)
	if err != nil || got == nil {
		t.Fatalf("get: (%v, %v)", got, err)
	}
	if got.Version != "0.5.0" || got.ManifestRevision != 7 || !got.LastSyncOK ||
		got.DisplayStatus != "active" || len(got.RecentErrors) != 1 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	hb, err := s.LastHeartbeat(ctx, did)
	if err != nil || hb == nil {
		t.Fatalf("heartbeat: (%v, %v)", hb, err)
	}
	if st := DeriveStatus(hb, got, time.Now().UTC()); st != StatusOnline {
		t.Fatalf("status = %q, want online", st)
	}

	// Sync failure flips an online frame to sync_error.
	h2 := *h
	h2.LastSyncOK = false
	if err := s.ReportHealth(ctx, did, &h2); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetHealth(ctx, did)
	if st := DeriveStatus(hb, got2, time.Now().UTC()); st != StatusSyncError {
		t.Fatalf("status = %q, want sync_error", st)
	}

	// Update states take precedence over everything.
	for state, want := range map[string]string{
		"downloading": StatusUpdating, "installing": StatusUpdating,
		"activating": StatusUpdating, "verifying": StatusUpdating,
		"failed": StatusUpdateFailed, "rolled_back": StatusRolledBack,
	} {
		h3 := *h
		h3.UpdateState = state
		if st := DeriveStatus(hb, &h3, time.Now().UTC()); st != want {
			t.Fatalf("state %q → %q, want %q", state, st, want)
		}
	}

	// Stale heartbeat → recently seen; very old → offline.
	old := time.Now().UTC().Add(-2 * time.Hour)
	if st := DeriveStatus(&old, got, time.Now().UTC()); st != StatusRecentlySeen {
		t.Fatalf("stale status = %q, want recently_seen", st)
	}
	ancient := time.Now().UTC().Add(-48 * time.Hour)
	if st := DeriveStatus(&ancient, got, time.Now().UTC()); st != StatusOffline {
		t.Fatalf("ancient status = %q, want offline", st)
	}
	if st := DeriveStatus(nil, nil, time.Now().UTC()); st != StatusOffline {
		t.Fatalf("never-seen status = %q, want offline", st)
	}
}

func TestHealthSanitizeBounds(t *testing.T) {
	h := &HealthReport{
		Version:       string(make([]byte, 500)),
		UptimeSeconds: -5,
		CPUTempC:      ptrF(500.0),
		RecentErrors:  make([]string, 20),
		MemTotalMB:    99999999,
	}
	for i := range h.RecentErrors {
		h.RecentErrors[i] = string(make([]byte, 500))
	}
	h.Sanitize()
	if len(h.Version) > 64 {
		t.Fatalf("version not bounded: %d", len(h.Version))
	}
	if h.UptimeSeconds != 0 {
		t.Fatalf("uptime not zeroed: %d", h.UptimeSeconds)
	}
	if h.CPUTempC != nil {
		t.Fatalf("out-of-range temp not nulled: %v", *h.CPUTempC)
	}
	if len(h.RecentErrors) != 10 {
		t.Fatalf("recent errors not capped to 10: %d", len(h.RecentErrors))
	}
	if len(h.RecentErrors[0]) > 200 {
		t.Fatalf("recent error not bounded: %d", len(h.RecentErrors[0]))
	}
	if h.MemTotalMB != 0 {
		t.Fatalf("mem not zeroed: %d", h.MemTotalMB)
	}
}

func TestListWithHealth(t *testing.T) {
	ctx := context.Background()
	dbh := newTestDB(t)
	s := NewStore(dbh)
	d1 := newDevice(t, s, "One")
	d2 := newDevice(t, s, "Two")

	if err := s.ReportHealth(ctx, d1, &HealthReport{Version: "0.5.0", LastSyncOK: true}); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListWithHealth(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	byID := map[string]*HealthWithDevice{}
	for _, w := range list {
		byID[w.ID] = w
	}
	if byID[d1].Status != StatusOnline || byID[d1].Health == nil {
		t.Fatalf("d1 = %+v", byID[d1])
	}
	if byID[d2].Status != StatusOffline || byID[d2].Health != nil {
		t.Fatalf("d2 = %+v", byID[d2])
	}
}

func TestRequestSync(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestDB(t))
	did := newDevice(t, s, "F")

	if seq, err := s.GetSyncRequestSeq(ctx, did); err != nil || seq != 0 {
		t.Fatalf("initial seq = (%d, %v)", seq, err)
	}
	for i := int64(1); i <= 3; i++ {
		seq, err := s.RequestSync(ctx, did)
		if err != nil || seq != i {
			t.Fatalf("request %d = (%d, %v)", i, seq, err)
		}
	}
	missing, _ := id.NewUUID()
	if _, err := s.RequestSync(ctx, missing); err != ErrNotFound {
		t.Fatalf("missing device = %v, want ErrNotFound", err)
	}
}

func TestFrameConfig(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestDB(t))
	did := newDevice(t, s, "F")

	// No config yet: zero document, version 0.
	cfg, err := s.GetFrameConfig(ctx, did)
	if err != nil || cfg.Version != 0 || cfg.Config.SlideshowIntervalSec != 0 {
		t.Fatalf("initial = (%+v, %v)", cfg, err)
	}

	bri := 42
	ok := true
	set, err := s.SetFrameConfig(ctx, did, FrameConfig{
		SlideshowIntervalSec: 90,
		Transition:           "crossfade",
		SleepOn:              "22:30",
		SleepOff:             "07:15",
		Brightness:           &bri,
		ShowClock:            &ok,
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if set.Version != 1 || set.Config.Transition != "crossfade" ||
		*set.Config.Brightness != 42 || !*set.Config.ShowClock {
		t.Fatalf("set result: %+v", set)
	}

	// Versions bump on every write.
	set2, err := s.SetFrameConfig(ctx, did, FrameConfig{SyncIntervalSec: 300})
	if err != nil || set2.Version != 2 || set2.Config.SyncIntervalSec != 300 {
		t.Fatalf("second set: (%+v, %v)", set2, err)
	}

	// Invalid documents are rejected and do not bump the version.
	for _, bad := range []FrameConfig{
		{SlideshowIntervalSec: 1},
		{Transition: "dissolve"},
		{TransitionDurationMS: 1},
		{SyncIntervalSec: 5},
		{SleepOn: "25:00"},
		{SleepOff: "7:00"},
		{Brightness: ptrF0(101)},
		{Orientation: "diagonal"},
	} {
		if _, err := s.SetFrameConfig(ctx, did, bad); err == nil {
			t.Fatalf("accepted invalid config %+v", bad)
		}
	}
	if cfg, _ := s.GetFrameConfig(ctx, did); cfg.Version != 2 {
		t.Fatalf("version bumped by rejected writes: %d", cfg.Version)
	}

	// Unknown device.
	missing, _ := id.NewUUID()
	if _, err := s.SetFrameConfig(ctx, missing, FrameConfig{}); err != ErrNotFound {
		t.Fatalf("missing device = %v", err)
	}
}

func TestFrameConfigJSONRoundtrip(t *testing.T) {
	bri := 77
	cfg := FrameConfig{
		SlideshowIntervalSec: 45,
		Transition:           "slide",
		TransitionDurationMS: 600,
		SyncIntervalSec:      120,
		SleepOn:              "23:00",
		SleepOff:             "06:30",
		Brightness:           &bri,
		Orientation:          "rotate90",
		ShowClock:            ptrB(false),
		ShowCaptions:         ptrB(true),
	}
	ctx := context.Background()
	s := NewStore(newTestDB(t))
	did := newDevice(t, s, "F")
	set, err := s.SetFrameConfig(ctx, did, cfg)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if *set.Config.Brightness != 77 || set.Config.TransitionDurationMS != 600 ||
		*set.Config.ShowClock != false || *set.Config.ShowCaptions != true ||
		set.Config.Orientation != "rotate90" {
		t.Fatalf("roundtrip: %+v", set.Config)
	}
}

func ptrF(v float64) *float64 { return &v }
func ptrB(v bool) *bool       { return &v }
func ptrF0(v int) *int        { return &v }
