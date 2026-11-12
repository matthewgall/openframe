package users

import (
	"context"
	"database/sql"
	"testing"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/migrations"
)

// newTestDB opens a temporary on-disk database and applies the real
// server migrations (faithful schema, exercises the migration runner).
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
