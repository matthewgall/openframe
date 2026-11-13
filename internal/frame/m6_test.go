package frame

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/frame/display"
	"github.com/matthewgall/openframe/internal/frame/sync"
	"github.com/matthewgall/openframe/internal/logging"
	migrations "github.com/matthewgall/openframe/migrations"
)

// newChannelTestFrame builds a minimal headless Frame on a temp DB,
// mirroring the M5 precedence test skeleton.
func newChannelTestFrame(t *testing.T) (*Frame, *config.Config) {
	t.Helper()
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
	cfg.DataDir = t.TempDir()

	flog, err := logging.New(logging.Options{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	drv, err := display.New(ctx, display.Options{Name: "headless", Width: 640, Height: 400, Log: flog})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drv.Close() })

	return &Frame{cfg: cfg, log: flog, db: fdb, drv: drv, errors: &errorRing{}, baseCfg: *cfg}, cfg
}

func readStateKey(t *testing.T, f *Frame, key string) string {
	t.Helper()
	v, ok, err := getState(context.Background(), f.db, key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return ""
	}
	return v
}

// TestM6UpdateChannelPrecedence verifies the M6 per-frame channel
// (spec §14): local settings > server config > config file, resolved
// to "stable" when unset and published to the state row the root
// updater reads.
func TestM6UpdateChannelPrecedence(t *testing.T) {
	ctx := context.Background()
	f, cfg := newChannelTestFrame(t)

	// Config file default: unset → resolves to stable.
	cfg.Frame.Update.Channel = ""
	if err := f.recomputeEffective(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readStateKey(t, f, StateKeyUpdateChannel); got != "stable" {
		t.Fatalf("channel = %q, want stable", got)
	}

	// Server config (admin UI) sets beta.
	f.applyServerConfig(sync.Config{Name: "Kitchen", UpdateChannel: "beta"})
	if f.cfg.Frame.Update.Channel != "beta" {
		t.Fatalf("effective channel = %q, want beta", f.cfg.Frame.Update.Channel)
	}
	if got := readStateKey(t, f, StateKeyUpdateChannel); got != "beta" {
		t.Fatalf("state channel = %q, want beta", got)
	}

	// Local setting (CLI) wins over the server.
	if err := SaveLocalSetting(ctx, f.db, settingUpdateChannel, "development"); err != nil {
		t.Fatal(err)
	}
	if err := f.recomputeEffective(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readStateKey(t, f, StateKeyUpdateChannel); got != "development" {
		t.Fatalf("state channel = %q, want development (local wins)", got)
	}

	// Clearing the local override falls back to the server value.
	if _, err := f.db.ExecContext(ctx,
		"DELETE FROM frame_settings WHERE key = ?", settingUpdateChannel); err != nil {
		t.Fatal(err)
	}
	if err := f.recomputeEffective(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readStateKey(t, f, StateKeyUpdateChannel); got != "beta" {
		t.Fatalf("state channel = %q, want beta after local clear", got)
	}
}

// TestM6HealthReportsUpdaterState verifies the frame surfaces the
// updater's state file in the health payload (unprivileged read-only
// integration, spec §14/§13).
func TestM6HealthReportsUpdaterState(t *testing.T) {
	ctx := context.Background()
	f, cfg := newChannelTestFrame(t)

	// No updater state visible → "none".
	if st := f.updateStatus(); st != "none" {
		t.Fatalf("update status = %q, want none", st)
	}

	// A committed updater state → the health mapping.
	_, _, updatesDir := cfg.Paths()
	if err := os.MkdirAll(updatesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := `{"phase":"idle","active_version":"1.2.0","last_error":"boom","last_update_at":"2026-02-09T01:00:00Z"}`
	if err := os.WriteFile(filepath.Join(updatesDir, "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := f.updateStatus(); st != "update_failed" {
		t.Fatalf("update status = %q, want update_failed", st)
	}

	// An in-flight phase → "updating".
	state = `{"phase":"downloading","target_version":"1.3.0"}`
	if err := os.WriteFile(filepath.Join(updatesDir, "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := f.updateStatus(); st != "updating" {
		t.Fatalf("update status = %q, want updating", st)
	}

	// The health payload carries it.
	h := f.buildHealth(ctx)
	if h.UpdateState != "updating" {
		t.Fatalf("health update_state = %q, want updating", h.UpdateState)
	}
}
