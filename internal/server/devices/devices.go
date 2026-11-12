// Package devices implements the device registry: assignment targets
// (M2) plus device credentials and manifest state for frame
// synchronisation (M3). Full pairing UX (challenge codes, admin
// approval) arrives in M5; M3 credentials are admin-issued.
package devices

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/id"
)

var ErrNotFound = errors.New("devices: not found")

// Device is the domain record.
type Device struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	AlbumCount    int        `json:"album_count"`
	HasCredential bool       `json:"has_credential"`
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
		        (SELECT COUNT(*) FROM album_devices ad WHERE ad.device_id = d.id),
		        (SELECT COUNT(*) FROM device_credentials c
		          WHERE c.device_id = d.id AND (c.revoked_at IS NULL OR c.revoked_at = '')) > 0
		   FROM devices d WHERE id = ?`, id).
		Scan(&d.ID, &d.Name, &created, &revoked, &d.AlbumCount, &d.HasCredential)
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
		        (SELECT COUNT(*) FROM album_devices ad WHERE ad.device_id = d.id),
		        (SELECT COUNT(*) FROM device_credentials c
		          WHERE c.device_id = d.id AND (c.revoked_at IS NULL OR c.revoked_at = '')) > 0
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
		if err := rows.Scan(&d.ID, &d.Name, &created, &revoked, &d.AlbumCount, &d.HasCredential); err != nil {
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

// ---------------------------------------------------------------------
// Device credentials (M3, spec §12: "Receive unique device credentials",
// "Support credential rotation and revocation").
//
// The raw token is returned to the caller exactly once at issue time
// and is never stored or logged; the database holds only its SHA-256.

// IssueCredential creates (or rotates) the device's credential and
// returns the raw token. Rotation invalidates the previous token
// immediately. Revoked devices cannot be issued credentials.
func (s *Store) IssueCredential(ctx context.Context, deviceID string) (string, error) {
	var revoked sql.NullString
	if err := s.dbh.QueryRowContext(ctx,
		"SELECT revoked_at FROM devices WHERE id = ?", deviceID).Scan(&revoked); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrNotFound
		}
		return "", err
	}
	if revoked.Valid && revoked.String != "" {
		return "", errors.New("devices: cannot issue credential to a revoked device")
	}

	var raw [43]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("issue credential: %w", err)
	}
	rawToken := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(sum[:])

	_, err := s.dbh.ExecContext(ctx,
		`INSERT INTO device_credentials (device_id, token_hash, issued_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(device_id) DO UPDATE SET
		   token_hash = excluded.token_hash, issued_at = excluded.issued_at, revoked_at = NULL`,
		deviceID, tokenHash, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return "", fmt.Errorf("issue credential: %w", err)
	}
	return rawToken, nil
}

// RevokeCredential revokes the device's current credential (the device
// stays registered; issue a new one to re-pair).
func (s *Store) RevokeCredential(ctx context.Context, deviceID string) error {
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE device_credentials SET revoked_at = ?
		 WHERE device_id = ? AND (revoked_at IS NULL OR revoked_at = '')`,
		time.Now().UTC().Format(time.RFC3339), deviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// HasLiveCredential reports whether the device holds an unrevoked
// credential (shown in the admin UI).
func (s *Store) HasLiveCredential(ctx context.Context, deviceID string) (bool, error) {
	var n int
	err := s.dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM device_credentials
		 WHERE device_id = ? AND (revoked_at IS NULL OR revoked_at = '')`, deviceID).Scan(&n)
	return n > 0, err
}

// VerifyCredential checks a raw bearer token against the device's
// stored credential hash. It reports a single opaque error for every
// failure mode (unknown device, no credential, revoked, mismatch) so
// responses do not distinguish them, and compares in constant time.
func (s *Store) VerifyCredential(ctx context.Context, deviceID, rawToken string) error {
	if rawToken == "" {
		return errors.New("devices: missing credential")
	}
	var (
		tokenHash   sql.NullString
		credRevoked sql.NullString
		devRevoked  sql.NullString
	)
	if err := s.dbh.QueryRowContext(ctx,
		`SELECT c.token_hash, c.revoked_at, d.revoked_at
		 FROM devices d LEFT JOIN device_credentials c ON c.device_id = d.id
		 WHERE d.id = ?`, deviceID).Scan(&tokenHash, &credRevoked, &devRevoked); err != nil {
		if err == sql.ErrNoRows {
			return errors.New("devices: bad credential")
		}
		return err
	}
	if devRevoked.Valid && devRevoked.String != "" {
		return errors.New("devices: bad credential")
	}
	if !tokenHash.Valid || tokenHash.String == "" {
		return errors.New("devices: bad credential")
	}
	if credRevoked.Valid && credRevoked.String != "" {
		return errors.New("devices: bad credential")
	}
	sum := sha256.Sum256([]byte(rawToken))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(tokenHash.String)) != 1 {
		return errors.New("devices: bad credential")
	}
	return nil
}

// tokenPrefix marks OpenFrame device tokens in logs-free contexts and
// lets operators recognise them. It is not a secret.
const tokenPrefix = "ofdv1_"

// ---------------------------------------------------------------------
// Manifest state (M3, spec §9 manifest protocol).

// ManifestState holds the device's last-built manifest and
// configuration fingerprints. Both revisions are content-derived:
// they advance only when the canonical content actually changes.
type ManifestState struct {
	ManifestRevision int64
	ManifestHash     string
	ConfigRevision   int64
	ConfigHash       string
}

func (s *Store) GetManifestState(ctx context.Context, deviceID string) (ManifestState, error) {
	var ms ManifestState
	err := s.dbh.QueryRowContext(ctx,
		`SELECT manifest_revision, manifest_hash, config_revision, config_hash
		   FROM devices WHERE id = ?`, deviceID).Scan(
		&ms.ManifestRevision, &ms.ManifestHash, &ms.ConfigRevision, &ms.ConfigHash)
	if err == sql.ErrNoRows {
		return ManifestState{}, ErrNotFound
	}
	if err != nil {
		return ManifestState{}, err
	}
	return ms, nil
}

// SetManifestState stores the fingerprints of the last built manifest.
func (s *Store) SetManifestState(ctx context.Context, deviceID string, ms ManifestState) error {
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE devices SET
		   manifest_revision = ?, manifest_hash = ?,
		   config_revision = ?, config_hash = ?
		 WHERE id = ?`,
		ms.ManifestRevision, ms.ManifestHash, ms.ConfigRevision, ms.ConfigHash, deviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------
// Device-scoped favourites and idempotent change ops (M3, spec §5/§9).

// Favourite upserts the device's favourite state for a photo with
// last-write-wins on the frame's change timestamp (opTS >= stored wins).
func (s *Store) SetFavourite(ctx context.Context, deviceID, photoHash string, value bool, opTS time.Time) error {
	ts := opTS.UTC().Format(time.RFC3339Nano)
	_, err := s.dbh.ExecContext(ctx,
		`INSERT INTO favourites (device_id, photo_hash, value, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(device_id, photo_hash) DO UPDATE SET
		   value = excluded.value, updated_at = excluded.updated_at
		 WHERE excluded.updated_at >= favourites.updated_at`,
		deviceID, photoHash, btoi(value), ts)
	return err
}

// Favourites returns the device's whole favourite map (for the
// manifest so a re-paired frame can restore its state).
func (s *Store) Favourites(ctx context.Context, deviceID string) (map[string]bool, error) {
	rows, err := s.dbh.QueryContext(ctx,
		"SELECT photo_hash, value FROM favourites WHERE device_id = ?", deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var h string
		var v int
		if err := rows.Scan(&h, &v); err != nil {
			return nil, err
		}
		out[h] = v == 1
	}
	return out, rows.Err()
}

// ClaimOp records (device, opID) as applied. It returns false when the
// op was already applied (idempotent replay).
func (s *Store) ClaimOp(ctx context.Context, deviceID, opID string) (bool, error) {
	res, err := s.dbh.ExecContext(ctx,
		`INSERT INTO device_ops (device_id, op_id, applied_at) VALUES (?, ?, ?)
		 ON CONFLICT(device_id, op_id) DO NOTHING`,
		deviceID, opID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
