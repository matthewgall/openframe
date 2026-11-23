package frame

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/config"
)

// The settings screen reads Last sync / Device from the frame's
// state tables. Both lookups once used non-existent table names
// ("sync"/"agent" instead of "sync_state"/"agent_state"): SQLite
// returned "no such table", the error was swallowed as "absent",
// and the info rows sat empty forever with no signal anywhere.
// These tests run against the REAL migrated schema so a table
// rename breaks them loudly.
func TestStateReadsUseRealTables(t *testing.T) {
	f, dbh := newTestFrame(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO sync_state (key, value, updated_at) VALUES (?, ?, ?)`,
		"device_name", "Granny", now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO sync_state (key, value, updated_at) VALUES (?, ?, ?)`,
		"last_success", now.Format(time.RFC3339Nano), now); err != nil {
		t.Fatal(err)
	}

	if got := f.deviceName(ctx); got != "Granny" {
		t.Errorf("deviceName = %q, want %q (sync_state write-through)", got, "Granny")
	}
	st := f.lastSyncTime(ctx)
	if st == nil || !st.Equal(now) {
		t.Errorf("lastSyncTime = %v, want %v (fallback path, engine nil)", st, now)
	}

	// agent_state fallback for the name when sync_state is empty.
	if _, err := dbh.ExecContext(ctx, `DELETE FROM sync_state WHERE key = 'device_name'`); err != nil {
		t.Fatal(err)
	}
	if err := setState(ctx, dbh, StateKeyDeviceName, "Hallway"); err != nil {
		t.Fatal(err)
	}
	if got := f.deviceName(ctx); got != "Hallway" {
		t.Errorf("deviceName = %q, want Hallway (agent_state fallback)", got)
	}

	// Info() surfaces free-space when the volume is readable. The
	// frame data dir must exist for statfs; build one under a real
	// filesystem rather than skipping.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "frame"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := *config.Default()
	cfg.DataDir = dir
	if got, ok := diskFree(cfg); !ok || got <= 0 {
		t.Errorf("diskFree = (%d,%v), want positive free bytes on a real volume", got, ok)
	}
}
