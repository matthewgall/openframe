package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Migration is a single ordered schema change.
type Migration struct {
	Version  int    `json:"version"`
	Name     string `json:"name"` // file name without extension
	SQL      string `json:"-"`
	Checksum string `json:"checksum"` // hex sha256 of SQL
}

var migrationFileRe = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// LoadMigrations reads all migration files from a subset directory of the
// embedded migrations filesystem (e.g. "server" or "frame") and returns
// them sorted by version. File names must be NNNN_description.sql.
func LoadMigrations(root fs.FS, subset string) ([]Migration, error) {
	entries, err := fs.ReadDir(root, subset)
	if err != nil {
		return nil, fmt.Errorf("read migrations %q: %w", subset, err)
	}
	var migs []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration %s/%s has invalid name (want NNNN_description.sql)", subset, e.Name())
		}
		ver, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migration %s/%s: %w", subset, e.Name(), err)
		}
		data, err := fs.ReadFile(root, path.Join(subset, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s/%s: %w", subset, e.Name(), err)
		}
		sum := sha256.Sum256(data)
		migs = append(migs, Migration{
			Version:  ver,
			Name:     m[2],
			SQL:      string(data),
			Checksum: fmt.Sprintf("%x", sum),
		})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].Version < migs[j].Version })
	return migs, nil
}

// migrateMu serialises Migrate calls within a process. SQLite in WAL
// mode lets concurrent writers proceed until commit, so a check-then-
// insert without this lock can race on the schema_migrations unique
// key. OpenFrame runs one process per database (systemd enforces it
// for deployments), which is the supported operating assumption.
var migrateMu sync.Mutex

// Migrate brings the database schema up to date.
//
// Guarantees:
//   - Migrations run in version order, each in its own transaction
//     (SQLite supports transactional DDL).
//   - Already-applied migrations are re-verified by checksum; a mismatch
//     is a hard error because it means files changed after deployment.
//   - Concurrent Migrate calls in this process are serialised.
func Migrate(ctx context.Context, dbh *sql.DB, migs []Migration) (applied []int, err error) {
	migrateMu.Lock()
	defer migrateMu.Unlock()

	if err := ensureSchemaTable(ctx, dbh); err != nil {
		return nil, err
	}

	appliedNow := make([]int, 0, len(migs))
	for _, m := range migs {
		state, err := migrationState(ctx, dbh, m.Version)
		if err != nil {
			return appliedNow, err
		}
		switch state.status {
		case "absent":
			if err := applyMigration(ctx, dbh, m); err != nil {
				return appliedNow, fmt.Errorf("apply migration %04d_%s: %w", m.Version, m.Name, err)
			}
			appliedNow = append(appliedNow, m.Version)
		case "applied":
			if state.checksum != m.Checksum {
				return appliedNow, fmt.Errorf("migration %04d_%s checksum mismatch: stored %s, file %s (files must not be edited after deployment)",
					m.Version, m.Name, state.checksum, m.Checksum)
			}
		}
	}
	return appliedNow, nil
}

// SchemaVersion returns the highest applied migration version (0 for an
// empty database).
func SchemaVersion(ctx context.Context, dbh *sql.DB) (int, error) {
	err := ensureSchemaTable(ctx, dbh)
	if err != nil {
		return 0, err
	}
	var v int
	err = dbh.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func ensureSchemaTable(ctx context.Context, dbh *sql.DB) error {
	_, err := dbh.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    checksum   TEXT    NOT NULL,
    applied_at TEXT    NOT NULL
)`)
	return err
}

type migrationRecord struct {
	status   string // "absent" or "applied"
	name     string
	checksum string
}

// migrationState returns the stored record for a version.
func migrationState(ctx context.Context, dbh *sql.DB, version int) (migrationRecord, error) {
	var rec migrationRecord
	var name, checksum sql.NullString
	err := dbh.QueryRowContext(ctx,
		"SELECT name, checksum FROM schema_migrations WHERE version = ?", version).Scan(&name, &checksum)
	if err == sql.ErrNoRows {
		rec.status = "absent"
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	return migrationRecord{status: "applied", name: name.String, checksum: checksum.String}, nil
}

// verifyChecksums re-checks every applied migration's checksum. Called by
// `openframe doctor`; also useful in tests.
func VerifyChecksums(ctx context.Context, dbh *sql.DB, migs []Migration) error {
	rows, err := dbh.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := map[int]string{}
	for rows.Next() {
		var v int
		var c string
		if err := rows.Scan(&v, &c); err != nil {
			return err
		}
		seen[v] = c
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range migs {
		c, ok := seen[m.Version]
		if !ok {
			continue
		}
		if c != m.Checksum {
			return fmt.Errorf("migration %04d_%s checksum mismatch: stored %s, file %s (files must not be edited after deployment)",
				m.Version, m.Name, c, m.Checksum)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, dbh *sql.DB, m Migration) error {
	tx, err := dbh.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
		m.Version, m.Name, m.Checksum, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

// MigrateFromFS is a convenience wrapper: load + migrate from the embedded
// migrations filesystem.
func MigrateFromFS(ctx context.Context, dbh *sql.DB, root fs.FS, subset string) ([]int, error) {
	migs, err := LoadMigrations(root, subset)
	if err != nil {
		return nil, err
	}
	return Migrate(ctx, dbh, migs)
}

// Describe renders applied migrations for human-facing output (status/doctor).
func Describe(appliedNow []int, total int) string {
	if len(appliedNow) == 0 {
		return fmt.Sprintf("schema up to date (%d migration(s))", total)
	}
	var b strings.Builder
	b.WriteString("applied ")
	for i, v := range appliedNow {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Itoa(v))
	}
	return b.String()
}
