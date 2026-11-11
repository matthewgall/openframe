package db

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"

	migrations "github.com/matthewgall/openframe/migrations"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbh, err := Open(context.Background(), t.TempDir()+"/test.db", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	return dbh
}

func TestLoadMigrationsEmbedded(t *testing.T) {
	for _, subset := range []string{"server", "frame"} {
		t.Run(subset, func(t *testing.T) {
			migs, err := LoadMigrations(migrations.FS(), subset)
			if err != nil {
				t.Fatal(err)
			}
			if len(migs) == 0 {
				t.Fatalf("no migrations loaded for %s", subset)
			}
			for i, m := range migs {
				if m.Version <= 0 || m.Name == "" || m.Checksum == "" {
					t.Errorf("migration %d incomplete: %+v", i, m)
				}
				if len(m.Checksum) != 64 {
					t.Errorf("checksum not sha256 hex: %q", m.Checksum)
				}
				if i > 0 && m.Version <= migs[i-1].Version {
					t.Errorf("migrations not ascending: %d after %d", m.Version, migs[i-1].Version)
				}
			}
		})
	}
}

func TestLoadMigrationsRejectsBadNames(t *testing.T) {
	mfs := fstest.MapFS{
		"test/good.sql":     &fstest.MapFile{Data: []byte("CREATE TABLE a(x);")},
		"test/1_init.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE b(x);")},
		"test/0001_bad.sql": &fstest.MapFile{Data: []byte("CREATE TABLE c(x);")},
	}
	if _, err := LoadMigrations(mfs, "test"); err == nil {
		t.Fatal("expected error for badly named migrations")
	}
}

func TestMigrateFreshAndIdempotent(t *testing.T) {
	ctx := context.Background()
	dbh := openTestDB(t)

	applied, err := MigrateFromFS(ctx, dbh, migrations.FS(), "frame")
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != 1 {
		t.Errorf("applied = %v, want [1]", applied)
	}

	// Second run: nothing to do, no error.
	applied, err = MigrateFromFS(ctx, dbh, migrations.FS(), "frame")
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Errorf("second run applied %v, want none", applied)
	}

	// Tables exist.
	var n int
	if err := dbh.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('identity','agent_state','meta','schema_migrations')").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("tables = %d, want 4", n)
	}

	v, err := SchemaVersion(ctx, dbh)
	if err != nil || v != 1 {
		t.Errorf("schema version = %d, %v; want 1", v, err)
	}
}

func TestMigrateMultipleOrdered(t *testing.T) {
	ctx := context.Background()
	dbh := openTestDB(t)

	mfs := fstest.MapFS{
		"test/0002_second.sql": &fstest.MapFile{Data: []byte("CREATE TABLE second(x INTEGER);")},
		"test/0001_first.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE first(x INTEGER);")},
	}
	migs, err := LoadMigrations(mfs, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately provided out of order on disk; runner must sort.
	if migs[0].Version != 1 || migs[1].Version != 2 {
		t.Fatalf("load order = %v", []int{migs[0].Version, migs[1].Version})
	}
	applied, err := Migrate(ctx, dbh, migs)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 || applied[0] != 1 || applied[1] != 2 {
		t.Errorf("applied = %v, want [1 2]", applied)
	}
	var n int
	dbh.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('first','second')").Scan(&n)
	if n != 2 {
		t.Errorf("tables = %d, want 2", n)
	}
}

func TestMigrateChecksumMismatchDetected(t *testing.T) {
	ctx := context.Background()
	dbh := openTestDB(t)

	sqlV1 := "CREATE TABLE mig(x INTEGER);"
	mfs := fstest.MapFS{
		"test/0001_a.sql": &fstest.MapFile{Data: []byte(sqlV1)},
	}
	if _, err := MigrateFromFS(ctx, dbh, mfs, "test"); err != nil {
		t.Fatal(err)
	}

	// "Edit" the file after deployment: checksum must now mismatch.
	mfs = fstest.MapFS{
		"test/0001_a.sql": &fstest.MapFile{Data: []byte(sqlV1 + "-- sneaky edit")},
	}
	_, err := MigrateFromFS(ctx, dbh, mfs, "test")
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
}

func TestMigrateFailsAtomically(t *testing.T) {
	ctx := context.Background()
	dbh := openTestDB(t)

	mfs := fstest.MapFS{
		// This migration fails part-way through: the first statement
		// succeeds, the second has a syntax error. Nothing may persist.
		"test/0001_bad.sql": &fstest.MapFile{Data: []byte("CREATE TABLE ok_table(x); CREATE TABL broken;")},
	}
	if _, err := MigrateFromFS(ctx, dbh, mfs, "test"); err == nil {
		t.Fatal("expected migration error")
	}
	var n int
	dbh.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='ok_table'").Scan(&n)
	if n != 0 {
		t.Error("partial migration persisted; DDL transaction not atomic")
	}
	v, _ := SchemaVersion(ctx, dbh)
	if v != 0 {
		t.Errorf("failed migration recorded, version = %d", v)
	}
}

func TestConcurrentMigrate(t *testing.T) {
	ctx := context.Background()
	dbh := openTestDB(t)
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := MigrateFromFS(ctx, dbh, migrations.FS(), "frame")
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestVerifyChecksums(t *testing.T) {
	ctx := context.Background()
	dbh := openTestDB(t)
	if _, err := MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		t.Fatal(err)
	}
	migs, _ := LoadMigrations(migrations.FS(), "server")
	if err := VerifyChecksums(ctx, dbh, migs); err != nil {
		t.Errorf("verify after fresh migrate: %v", err)
	}
}
