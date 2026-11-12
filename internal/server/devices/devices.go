// Package devices implements the minimal device registry (M2).
//
// M2 needs frames only as assignment targets for albums (spec §5).
// Pairing, credentials, health and remote sync control arrive in M5;
// the schema row is intentionally small and additive from there.
package devices

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/id"
)

var ErrNotFound = errors.New("devices: not found")

// Device is the domain record.
type Device struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	AlbumCount int        `json:"album_count"`
}

// Store persists devices.
type Store struct{ dbh *sql.DB }

func NewStore(dbh *sql.DB) *Store { return &Store{dbh: dbh} }

// Create registers a device by name (admin action in M2; replaced by
// the pairing flow in M5).
func (s *Store) Create(ctx context.Context, name string) (*Device, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return nil, errors.New("devices: invalid name")
	}
	did, err := id.NewUUID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.dbh.ExecContext(ctx,
		`INSERT INTO devices (id, name, created_at) VALUES (?, ?, ?)`,
		did, name, now); err != nil {
		return nil, err
	}
	return s.Get(ctx, did)
}

func (s *Store) Get(ctx context.Context, id string) (*Device, error) {
	var d Device
	var revoked sql.NullString
	var created string
	err := s.dbh.QueryRowContext(ctx,
		`SELECT id, name, created_at, revoked_at,
		        (SELECT COUNT(*) FROM album_devices ad WHERE ad.device_id = d.id)
		   FROM devices d WHERE id = ?`, id).
		Scan(&d.ID, &d.Name, &created, &revoked, &d.AlbumCount)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.CreatedAt = parseTS(created)
	if revoked.Valid && revoked.String != "" {
		t := parseTS(revoked.String)
		d.RevokedAt = &t
	}
	return &d, nil
}

func (s *Store) List(ctx context.Context) ([]Device, error) {
	rows, err := s.dbh.QueryContext(ctx,
		`SELECT d.id, d.name, d.created_at, d.revoked_at,
		        (SELECT COUNT(*) FROM album_devices ad WHERE ad.device_id = d.id)
		   FROM devices d ORDER BY d.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		var revoked sql.NullString
		var created string
		if err := rows.Scan(&d.ID, &d.Name, &created, &revoked, &d.AlbumCount); err != nil {
			return nil, err
		}
		d.CreatedAt = parseTS(created)
		if revoked.Valid && revoked.String != "" {
			t := parseTS(revoked.String)
			d.RevokedAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Rename updates the display name.
func (s *Store) Rename(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("devices: invalid name")
	}
	res, err := s.dbh.ExecContext(ctx, `UPDATE devices SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Revoke marks a device's credentials as revoked (M5 enforces this on
// auth; M2 records the state).
func (s *Store) Revoke(ctx context.Context, id string) error {
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE devices SET revoked_at = ? WHERE id = ? AND (revoked_at IS NULL OR revoked_at = '')`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
