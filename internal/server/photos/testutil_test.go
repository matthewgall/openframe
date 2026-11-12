package photos

import (
	"context"
	"database/sql"
	"testing"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/migrations"
)

// newTestDB opens a temporary database with the real server migrations
// and a seeded uploader user.
func newTestDB(t *testing.T) (*sql.DB, string) {
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

	uid, err := insertUser(ctx, dbh, "uploader@example.com")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return dbh, uid
}

func insertUser(ctx context.Context, dbh *sql.DB, email string) (string, error) {
	uid := "00000000-0000-0000-0000-" + email[:8]
	_, err := dbh.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, role, password_hash, created_at, updated_at)
		 VALUES (?, ?, 'Test', 'member', 'x', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		uid, email)
	return uid, err
}
