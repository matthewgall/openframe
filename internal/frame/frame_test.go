package frame

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/frame/display"
	migrations "github.com/matthewgall/openframe/migrations"
)

func newTestFrame(t *testing.T) (*Frame, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	dbh, err := db.Open(ctx, filepath.Join(t.TempDir(), "frame.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "frame"); err != nil {
		t.Fatal(err)
	}
	drv, err := display.New(ctx, display.Options{
		Name: "headless", Width: 1280, Height: 800,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drv.Close() })
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	f, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), dbh, drv)
	if err != nil {
		t.Fatal(err)
	}
	return f, dbh
}

func TestIdentityCreatedOnce(t *testing.T) {
	ctx := context.Background()
	_, dbh := newTestFrame(t)

	id1, err := EnsureIdentity(ctx, dbh)
	if err != nil {
		t.Fatal(err)
	}
	if len(id1) != 36 {
		t.Errorf("identity = %q, want uuid format", id1)
	}
	id2, err := EnsureIdentity(ctx, dbh)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("identity changed: %q vs %q", id1, id2)
	}
}

func TestIdentityPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "frame.db")

	id1 := runFrameUntilStopped(t, dir, dbPath)

	// "Restart": open the same database again.
	dbh2, err := db.Open(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dbh2.Close()
	id2, err := EnsureIdentity(ctx, dbh2)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("identity not stable across restart: %q vs %q", id1, id2)
	}
}

// runFrameUntilStopped starts a real frame (headless) and stops it after
// a short run, returning the device identity.
func runFrameUntilStopped(t *testing.T, dir, dbPath string) string {
	t.Helper()
	ctx := context.Background()
	dbh, err := db.Open(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "frame"); err != nil {
		t.Fatal(err)
	}
	drv, err := display.New(ctx, display.Options{
		Name: "headless", Width: 640, Height: 480,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dir
	f, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), dbh, drv)
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.Run(runCtx) }()

	// Let it record identity + first heartbeat, then stop.
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(50 * time.Millisecond)
		var hb string
		err := dbh.QueryRow("SELECT value FROM agent_state WHERE key='last_heartbeat'").Scan(&hb)
		if err == nil && hb != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat recorded")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frame did not stop")
	}
	drv.Close()
	dbh.Close()
	return f.Identity()
}

func TestRunMarksState(t *testing.T) {
	f, dbh := newTestFrame(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	var running, hb string
	dbh.QueryRow("SELECT value FROM agent_state WHERE key='running'").Scan(&running)
	if running != "false" {
		t.Errorf("running after stop = %q, want false", running)
	}
	if err := dbh.QueryRow("SELECT value FROM agent_state WHERE key='last_heartbeat'").Scan(&hb); err != nil {
		t.Errorf("last_heartbeat missing: %v", err)
	}
}

func TestComponentFailureStopsOthers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newRunner(log)

	sawCancel := make(chan struct{})
	slow := Component{Name: "slow", Run: func(ctx context.Context) error {
		<-ctx.Done()
		close(sawCancel)
		return nil
	}}
	fastFail := Component{Name: "fastfail", Run: func(ctx context.Context) error {
		return errors.New("boom")
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := r.Run(ctx, []Component{fastFail, slow})
	if err == nil || err.Error() != "boom" {
		t.Errorf("Run error = %v, want boom", err)
	}
	select {
	case <-sawCancel:
	case <-time.After(2 * time.Second):
		t.Error("slow component was not cancelled")
	}
}

func TestRunnerTooManyComponents(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newRunner(log)
	var comps []Component
	for i := 0; i < r.maxComponents+1; i++ {
		comps = append(comps, Component{Name: string(rune('a' + i)), Run: func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Run(ctx, comps); err == nil {
		t.Error("expected error for too many components")
	}
}
