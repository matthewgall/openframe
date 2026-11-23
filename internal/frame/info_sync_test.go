package frame

import (
	"context"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/frame/cache"
	"github.com/matthewgall/openframe/internal/frame/sync"
	"github.com/matthewgall/openframe/internal/logging"
)

// "Last sync" must mean the last successful manifest CHECK (what the
// admin UI reports as health.last_sync_at), not the last collection
// ACTIVATION - a healthy frame with an unchanged library checks in
// constantly while last_success stays frozen for days.
func TestInfoLastSyncIsLastCheck(t *testing.T) {
	ctx := context.Background()
	f, dbh := newTestFrame(t)

	identity, err := EnsureIdentity(ctx, dbh)
	if err != nil {
		t.Fatal(err)
	}
	flog, err := logging.New(logging.Options{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	// Nothing listens on port 1: the check fails fast but STILL
	// records the attempt + error, exactly like a flaky network.
	eng, err := sync.New(identity, "http://127.0.0.1:1", "", flog, dbh, c, &cfg.Frame)
	if err != nil {
		t.Fatal(err)
	}
	f.engine = eng

	// An ancient activation timestamp must NOT be the display value.
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO sync_state (key, value, updated_at) VALUES ('last_success', ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		"2020-01-01T00:00:00Z", time.Now()); err != nil {
		t.Fatal(err)
	}

	eng.Check(ctx) // fails against the dead port

	before := time.Now().Add(-2 * time.Second)
	info := (&frameData{f: f}).Info(ctx)
	if info.LastSync == nil {
		t.Fatal("LastSync nil after a check attempt")
	}
	if info.LastSync.Before(before) {
		t.Errorf("LastSync = %v, want the ATTEMPT time (now), not the 2020 activation", info.LastSync)
	}
	if info.LastSyncError == "" {
		t.Error("LastSyncError empty after a failed check")
	}

	// A successful check (304 path would also qualify): error clears,
	// the attempt time moves forward, activation stays ancient.
	if _, err := dbh.ExecContext(ctx,
		`UPDATE sync_state SET value = '' WHERE key = 'last_error'`); err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.ExecContext(ctx,
		`UPDATE sync_state SET value = ? WHERE key = 'last_attempt'`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	info = (&frameData{f: f}).Info(ctx)
	if info.LastSyncError != "" {
		t.Errorf("LastSyncError = %q after cleared state", info.LastSyncError)
	}
	if info.LastSync == nil || info.LastSync.Before(before) {
		t.Errorf("LastSync = %v after successful attempt", info.LastSync)
	}
}
