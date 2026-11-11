package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"time"

	"github.com/openframe/openframe/internal/config"
	"github.com/openframe/openframe/internal/db"
	"github.com/openframe/openframe/internal/frame"
	migrations "github.com/openframe/openframe/migrations"
)

// frameState is what pair/status/doctor can report about the local frame.
type frameState struct {
	Available        bool       `json:"available"`
	DatabasePath     string     `json:"database_path"`
	DeviceID         string     `json:"device_id,omitempty"`
	Running          bool       `json:"running"`
	LastHeartbeat    *time.Time `json:"last_heartbeat,omitempty"`
	AppVersion       string     `json:"app_version,omitempty"`
	LastSyncError    string     `json:"last_sync_error,omitempty"`
	PairingStatus    string     `json:"pairing_status"` // unconfigured | pending | unknown
	ServerConfigured bool       `json:"server_configured"`
}

// readFrameState opens the frame database read-only (never creating it)
// and summarises local frame state.
func readFrameState(ctx context.Context, cfg *config.Config) frameState {
	_, frameDir, _ := cfg.Paths()
	dbPath := filepath.Join(frameDir, "frame.db")
	st := frameState{
		Available:        true,
		DatabasePath:     dbPath,
		ServerConfigured: cfg.Frame.ServerURL != "",
		PairingStatus:    "unconfigured",
	}
	if _, err := os.Stat(dbPath); err != nil {
		st.Available = false
		st.PairingStatus = "unconfigured"
		return st
	}

	dbh, err := db.Open(ctx, dbPath, true)
	if err != nil {
		st.Available = false
		return st
	}
	defer dbh.Close()

	var id string
	if err := dbh.QueryRowContext(ctx, "SELECT id FROM identity LIMIT 1").Scan(&id); err == nil {
		st.DeviceID = id
		st.PairingStatus = "pending"
	}

	running, err := stValue(ctx, dbh, "running")
	if err == nil && running == "true" {
		st.Running = true
	}
	if hb, err := stValue(ctx, dbh, "last_heartbeat"); err == nil {
		if t, perr := time.Parse(time.RFC3339Nano, hb); perr == nil {
			st.LastHeartbeat = &t
		}
	}
	if v, err := stValue(ctx, dbh, "app_version"); err == nil {
		st.AppVersion = v
	}
	if v, err := stValue(ctx, dbh, "last_sync_error"); err == nil {
		st.LastSyncError = v
	}
	return st
}

func stValue(ctx context.Context, dbh *sql.DB, key string) (string, error) {
	var v string
	err := dbh.QueryRowContext(ctx, "SELECT value FROM agent_state WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", err
	}
	return v, err
}

// initFrameState opens (creating if needed) and migrates the frame
// database, then ensures an identity exists. Used by `openframe pair`.
func initFrameState(ctx context.Context, cfg *config.Config) (*sql.DB, string, error) {
	_, frameDir, _ := cfg.Paths()
	dbPath := filepath.Join(frameDir, "frame.db")
	dbh, err := db.Open(ctx, dbPath, false)
	if err != nil {
		return nil, "", err
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "frame"); err != nil {
		dbh.Close()
		return nil, "", err
	}
	id, err := frame.EnsureIdentity(ctx, dbh)
	if err != nil {
		dbh.Close()
		return nil, "", err
	}
	return dbh, id, nil
}
