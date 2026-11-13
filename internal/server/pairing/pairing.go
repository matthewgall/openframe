// Package pairing implements the device pairing protocol (spec §12):
//
//  1. A new frame (unpaired) requests a pairing challenge from the
//     server over its outbound HTTPS connection.
//  2. The server generates a short-lived, single-use code and returns
//     it; the frame displays the code.
//  3. An administrator approves in the admin UI by entering the code
//     shown on the frame. Approval atomically registers the device
//     (under the frame's own identity) and issues its credential.
//
// Brute-force posture: the code is 8 characters from a 31-symbol
// unambiguous alphabet (~40 bits of entropy), checked in constant
// time, and challenge *creation* is rate-limited per source address so
// a rogue frame cannot flood the admin UI or the pairing table.
// Approval itself requires an authenticated admin session, so guessing
// a code has no privileged effect by itself.
package pairing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/id"
	"github.com/matthewgall/openframe/internal/server/devices"
)

const (
	// ChallengeTTL bounds how long an unapproved code stays valid.
	ChallengeTTL = 15 * time.Minute
	// MaxPendingPerIP bounds concurrent pending challenges per source
	// address (flood control, spec §12 "prevent brute-force pairing").
	MaxPendingPerIP = 3
	// MaxCreatedPerWindow bounds challenge creations per address per
	// window (a frame that restarts in a loop cannot exhaust the table).
	MaxCreatedPerWindow = 6
	creationWindow      = 30 * time.Minute

	codeLen = 8
	// codeAlphabet omits 0/O, 1/I and L: the code is typed in by a
	// human, usually from a photo of the screen.
	codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
)

// Status values for a challenge.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
	StatusExpired  = "expired"
)

var (
	ErrNotFound   = errors.New("pairing: challenge not found")
	ErrWrongCode  = errors.New("pairing: incorrect code")
	ErrNotPending = errors.New("pairing: challenge already consumed")
	ErrRateLimit  = errors.New("pairing: too many pairing requests; try again later")
	ErrExists     = errors.New("pairing: device already registered")
)

// Challenge is the admin-visible view. The code itself never appears
// (only on the frame's screen); the server stores its hash.
type Challenge struct {
	ID         string    `json:"id"`
	DeviceID   string    `json:"device_id"`
	DeviceName string    `json:"device_name"`
	SourceIP   string    `json:"source_ip"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Store persists pairing challenges.
type Store struct{ dbh *sql.DB }

func NewStore(dbh *sql.DB) *Store { return &Store{dbh: dbh} }

// LatestForDevice returns the device's most recent challenge (any
// status) or nil when it has never paired. Used for the restart
// recovery path: a frame that was approved but lost power before
// claiming its credential gets the challenge id back on a 409 so it
// can resume polling and claim the still-pending one-time credential.
func (s *Store) LatestForDevice(ctx context.Context, deviceID string) (*Challenge, error) {
	var c Challenge
	var createdAt, expiresAt string
	err := s.dbh.QueryRowContext(ctx,
		`SELECT id, device_id, device_name, source_ip, status, created_at, expires_at
		 FROM pairing_challenges WHERE device_id = ?
		 ORDER BY created_at DESC LIMIT 1`, deviceID).Scan(
		&c.ID, &c.DeviceID, &c.DeviceName, &c.SourceIP, &c.Status,
		&createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pairing latest for device: %w", err)
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	c.ExpiresAt, _ = time.Parse(time.RFC3339, expiresAt)
	return &c, nil
}

// Created is the full result of a challenge creation, including the
// one-time code shown to the frame.
type Created struct {
	Challenge
	Code string `json:"code"`
}

// Create generates a pairing challenge for the given frame identity.
// deviceID must be the frame's own UUID (created by `openframe pair`);
// it becomes the device ID on approval, so the frame needs no
// re-identity later.
func (s *Store) Create(ctx context.Context, deviceID, deviceName, sourceIP string) (*Created, error) {
	deviceID = strings.ToLower(strings.TrimSpace(deviceID))
	deviceName = strings.TrimSpace(deviceName)
	if !id.IsValidUUID(deviceID) {
		return nil, errors.New("pairing: invalid device id")
	}
	if deviceName == "" || len(deviceName) > 100 {
		deviceName = "Frame"
	}

	// A frame that is already registered should not be pairing again;
	// the admin UI manages that device directly.
	var n int
	if err := s.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM devices WHERE id = ?", deviceID).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrExists
	}

	if err := s.Sweep(ctx); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var pending int
	if err := s.dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pairing_challenges
		  WHERE source_ip = ? AND status = 'pending'`, sourceIP).Scan(&pending); err != nil {
		return nil, err
	}
	if pending >= MaxPendingPerIP {
		return nil, ErrRateLimit
	}
	var created int
	if err := s.dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pairing_challenges
		  WHERE source_ip = ? AND created_at >= ?`,
		sourceIP, now.Add(-creationWindow).Format(time.RFC3339)).Scan(&created); err != nil {
		return nil, err
	}
	if created >= MaxCreatedPerWindow {
		return nil, ErrRateLimit
	}

	chID, err := id.NewUUID()
	if err != nil {
		return nil, err
	}
	code, err := newCode()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(code))
	expires := now.Add(ChallengeTTL)

	_, err = s.dbh.ExecContext(ctx,
		`INSERT INTO pairing_challenges
		 (id, device_id, device_name, code_hash, source_ip, status, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`,
		chID, deviceID, deviceName, hex.EncodeToString(sum[:]), sourceIP,
		now.Format(time.RFC3339), expires.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("pairing create: %w", err)
	}
	return &Created{
		Challenge: Challenge{
			ID:         chID,
			DeviceID:   deviceID,
			DeviceName: deviceName,
			SourceIP:   sourceIP,
			Status:     StatusPending,
			CreatedAt:  now,
			ExpiresAt:  expires,
		},
		Code: code,
	}, nil
}

// ListPending returns unexpired pending challenges, oldest first.
func (s *Store) ListPending(ctx context.Context) ([]Challenge, error) {
	if err := s.Sweep(ctx); err != nil {
		return nil, err
	}
	rows, err := s.dbh.QueryContext(ctx,
		`SELECT id, device_id, device_name, source_ip, status, created_at, expires_at
		   FROM pairing_challenges WHERE status = 'pending'
		  ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Challenge{}
	for rows.Next() {
		var (
			c       Challenge
			created string
			expired string
		)
		if err := rows.Scan(&c.ID, &c.DeviceID, &c.DeviceName, &c.SourceIP,
			&c.Status, &created, &expired); err != nil {
			return nil, err
		}
		c.CreatedAt, _ = time.Parse(time.RFC3339, created)
		c.ExpiresAt, _ = time.Parse(time.RFC3339, expired)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Status returns the frame-visible state of a challenge: pending,
// approved (with the device ID), rejected, or expired. When the
// challenge was approved and its credential has not yet been claimed,
// the credential is returned AND the stored copy is nulled — a
// one-time claim, so the raw token exists at rest only between
// approval and the frame's next poll.
func (s *Store) Status(ctx context.Context, id string) (status, deviceID, credential string, err error) {
	if err := s.Sweep(ctx); err != nil {
		return "", "", "", err
	}
	var cred sql.NullString
	err = s.dbh.QueryRowContext(ctx,
		`SELECT status, COALESCE(approved_device_id, ''), credential
		   FROM pairing_challenges WHERE id = ?`, id).Scan(&status, &deviceID, &cred)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", ErrNotFound
	}
	if err != nil {
		return "", "", "", err
	}
	if status == StatusApproved && cred.Valid && cred.String != "" {
		if _, err := s.dbh.ExecContext(ctx,
			`UPDATE pairing_challenges SET credential = NULL WHERE id = ? AND credential IS NOT NULL`,
			id); err != nil {
			return "", "", "", err
		}
		credential = cred.String
	}
	return status, deviceID, credential, nil
}

// Approve verifies the code in constant time and, in one transaction,
// registers the device under its frame identity and issues its
// credential. Returns the device ID and the one-time credential.
func (s *Store) Approve(ctx context.Context, id, code string) (deviceID, token string, err error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != codeLen {
		return "", "", ErrWrongCode
	}
	sum := sha256.Sum256([]byte(code))
	wantHash := hex.EncodeToString(sum[:])

	tx, err := s.dbh.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	var (
		cid       string
		cdev      string
		status    string
		expiresAt string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, device_id, status, expires_at FROM pairing_challenges WHERE id = ?`,
		id).Scan(&cid, &cdev, &status, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if status == StatusPending {
		exp, perr := time.Parse(time.RFC3339, expiresAt)
		if perr != nil || time.Now().UTC().After(exp) {
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE pairing_challenges SET status = 'expired' WHERE id = ?`, id); uerr != nil {
				return "", "", uerr
			}
			return "", "", ErrNotFound
		}
	} else {
		return "", "", ErrNotPending
	}

	var storedHash string
	if err := tx.QueryRowContext(ctx,
		"SELECT code_hash FROM pairing_challenges WHERE id = ?", id).
		Scan(&storedHash); err != nil {
		return "", "", err
	}
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(wantHash)) != 1 {
		return "", "", ErrWrongCode
	}

	// The device may have been registered manually (admin UI) in the
	// meantime; refuse rather than clobber.
	var name string
	if err := tx.QueryRowContext(ctx,
		"SELECT device_name FROM pairing_challenges WHERE id = ?", id).Scan(&name); err != nil {
		return "", "", err
	}
	var existing int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM devices WHERE id = ?", cdev).Scan(&existing); err != nil {
		return "", "", err
	}
	if existing > 0 {
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE pairing_challenges SET status = 'rejected',
			   consumed_at = ? WHERE id = ?`,
			time.Now().UTC().Format(time.RFC3339), id); uerr != nil {
			return "", "", uerr
		}
		return "", "", ErrExists
	}

	rawToken, err := devices.NewToken()
	if err != nil {
		return "", "", err
	}
	tsum := sha256.Sum256([]byte(rawToken))
	now := time.Now().UTC().Format(time.RFC3339)

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO devices (id, name, created_at) VALUES (?, ?, ?)`,
		cdev, name, now); err != nil {
		return "", "", fmt.Errorf("pairing approve: create device: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_credentials (device_id, token_hash, issued_at)
		 VALUES (?, ?, ?)`,
		cdev, hex.EncodeToString(tsum[:]), now); err != nil {
		return "", "", fmt.Errorf("pairing approve: issue credential: %w", err)
	}
	// The credential is kept on the challenge row (transient) so the
	// frame can claim it on its next status poll; Status nulls it
	// after the first claim. See the table comment in migration 0004.
	if _, err := tx.ExecContext(ctx,
		`UPDATE pairing_challenges
		   SET status = 'approved', consumed_at = ?, approved_device_id = ?,
	       credential = ?
		 WHERE id = ?`, now, cdev, rawToken, id); err != nil {
		return "", "", err
	}
	return cdev, rawToken, tx.Commit()
}

// Reject marks a pending challenge as rejected (admin declined).
func (s *Store) Reject(ctx context.Context, id string) error {
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE pairing_challenges SET status = 'rejected', consumed_at = ?
		  WHERE id = ? AND status = 'pending'`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Sweep marks pending challenges past their expiry as expired. Cheap
// to run on every read path.
func (s *Store) Sweep(ctx context.Context) error {
	_, err := s.dbh.ExecContext(ctx,
		`UPDATE pairing_challenges
		   SET status = 'expired'
		 WHERE status = 'pending' AND expires_at <= ?`,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// newCode generates a pairing code from the unambiguous alphabet.
func newCode() (string, error) {
	b := make([]byte, codeLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, codeLen)
	for i, x := range b {
		out[i] = codeAlphabet[int(x)%len(codeAlphabet)]
	}
	return string(out), nil
}
