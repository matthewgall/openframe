package frame

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/matthewgall/openframe/internal/id"
)

// EnsureIdentity returns the stable device identity, creating it on first
// boot. The identity is a UUIDv4 generated locally (spec §12: frames
// initiate all communication; no inbound connections are ever required).
func EnsureIdentity(ctx context.Context, dbh *sql.DB) (string, error) {
	var existing string
	err := dbh.QueryRowContext(ctx, "SELECT id FROM identity LIMIT 1").Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("read identity: %w", err)
	}

	uid, err := id.NewUUID()
	if err != nil {
		return "", err
	}
	if _, err := dbh.ExecContext(ctx,
		"INSERT INTO identity (id, created_at) VALUES (?, ?)",
		uid, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return "", fmt.Errorf("insert identity: %w", err)
	}
	return uid, nil
}

// setState writes a key/value into agent_state (upsert).
func setState(ctx context.Context, dbh *sql.DB, key, value string) error {
	_, err := dbh.ExecContext(ctx,
		`INSERT INTO agent_state (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("set agent_state %s: %w", key, err)
	}
	return nil
}

// getState reads a key from agent_state; ok=false when absent.
func getState(ctx context.Context, dbh *sql.DB, key string) (value string, ok bool, err error) {
	err = dbh.QueryRowContext(ctx, "SELECT value FROM agent_state WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}
