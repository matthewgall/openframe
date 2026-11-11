// Package db provides SQLite access for both the server and the frame.
//
// The driver is modernc.org/sqlite (pure Go), chosen so that the server
// and frame binaries cross-compile for linux/arm64 without a C toolchain
// on the build host (ADR-0002).
//
// Every OpenFrame database runs in WAL mode with foreign keys enabled.
// Busy handling is delegated to SQLite via busy_timeout rather than
// application-level retry loops.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// Open opens (creating if necessary) the SQLite database at path and
// returns a ready-to-use pool. The parent directory is created with
// 0700; the database file ends up 0600.
//
// readOnly opens with file:mode=ro and no WAL checkpoint side effects.
func Open(ctx context.Context, path string, readOnly bool) (*sql.DB, error) {
	if !readOnly {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}

	// SQLite URI open modes: rwc = read-write, create if missing (the
	// standard "rw" mode refuses to create and is wrong for first boot);
	// ro = read-only, must exist (used by doctor/status probes).
	mode := "rwc"
	if readOnly {
		mode = "ro"
	}
	// Per-connection pragmas live in the DSN so every pooled connection
	// inherits them. WAL + busy_timeout give us one writer with
	// non-blocking readers; _txlock=immediate reduces write-transaction
	// SQLITE_BUSY races (a modernc driver-specific option).
	dsn := fmt.Sprintf(
		"file:%s?mode=%s&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate",
		path, mode)

	dbh, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	// Bounded pool: a handful of connections keeps memory predictable on
	// 1 GB Pi hardware while allowing the M2+ API to serve concurrent
	// readers alongside a single writer.
	dbh.SetMaxOpenConns(4)
	dbh.SetMaxIdleConns(2)
	dbh.SetConnMaxIdleTime(5 * time.Minute)

	if err := dbh.PingContext(ctx); err != nil {
		dbh.Close()
		return nil, fmt.Errorf("connect sqlite %s: %w", path, err)
	}
	return dbh, nil
}

// Ping runs a fast liveness probe bounded by ctx.
func Ping(ctx context.Context, dbh *sql.DB) error {
	return dbh.PingContext(ctx)
}

// IntegrityCheck runs PRAGMA integrity_check and reports whether the
// database is structurally sound. Used by `openframe doctor`.
func IntegrityCheck(ctx context.Context, dbh *sql.DB) (string, error) {
	var result string
	err := dbh.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result)
	if err != nil {
		return "", err
	}
	return result, nil
}
