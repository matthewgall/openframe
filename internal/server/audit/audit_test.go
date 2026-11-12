package audit

import (
	"context"
	"log/slog"
	"testing"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/migrations"
)

func TestActionAndList(t *testing.T) {
	ctx := context.Background()
	dbh, err := db.Open(ctx, t.TempDir()+"/test.db", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer dbh.Close()

	r := NewRecorder(dbh, slog.New(slog.NewTextHandler(discard{}, nil)))

	r.Action(ctx, "actor-1", "user.created", "user", "target-1", "10.0.0.1", map[string]string{"email": "a@b.c"})
	r.Action(ctx, "", "login.failed", "user", "target-1", "10.0.0.2", nil)

	entries, err := r.List(ctx, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: %d", len(entries))
	}
	// Most recent first.
	if entries[0].Action != "login.failed" || entries[1].Action != "user.created" {
		t.Fatalf("ordering: %+v", entries)
	}
	if entries[0].IP != "10.0.0.2" {
		t.Fatalf("ip: %s", entries[0].IP)
	}
	if entries[1].ActorUserID != "actor-1" {
		t.Fatalf("actor: %s", entries[1].ActorUserID)
	}
	if entries[1].Detail == "" || entries[1].Detail == "null" {
		t.Fatalf("detail: %q", entries[1].Detail)
	}
}

func TestActionNeverPanicsOnBadDetail(t *testing.T) {
	r := NewRecorder(nil, slog.New(slog.NewTextHandler(discard{}, nil)))
	// nil dbh: recording must degrade gracefully (audit must not take
	// down the request path).
	r.Action(context.Background(), "a", "x.y", "t", "id", "1.2.3.4", map[string]any{"k": 1})
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
