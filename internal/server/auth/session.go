package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/matthewgall/openframe/internal/id"
)

// SessionError classifies validation failures so the API layer can map
// them to the right status code without string matching.
var (
	ErrSessionNotFound = errors.New("auth: session not found")
	ErrSessionExpired  = errors.New("auth: session expired")
	ErrSessionRevoked  = errors.New("auth: session revoked")
)

// Info is the validated, public view of a session.
type Info struct {
	SessionID string
	UserID    string
	ExpiresAt time.Time
	Token     string // plaintext, in-memory only; never persisted
}

// Manager handles database-backed sessions.
type Manager struct {
	dbh  *sql.DB
	ttl  time.Duration
	nowf func() time.Time
}

// NewManager builds a session manager.
func NewManager(dbh *sql.DB, ttl time.Duration) *Manager {
	return &Manager{dbh: dbh, ttl: ttl, nowf: time.Now}
}

// Create issues a new session for the user and returns it (the Token
// field must be delivered to the client exactly once).
func (m *Manager) Create(ctx context.Context, userID, userAgent, ip string) (*Info, error) {
	sid, err := id.NewUUID()
	if err != nil {
		return nil, err
	}
	token, err := NewToken()
	if err != nil {
		return nil, err
	}
	now := m.nowf().UTC()
	if _, err := m.dbh.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, token_hash, user_agent, ip, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sid, userID, TokenHash(token), userAgent, ip,
		now.Format(time.RFC3339), now.Add(m.ttl).Format(time.RFC3339)); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &Info{SessionID: sid, UserID: userID, ExpiresAt: now.Add(m.ttl), Token: token}, nil
}

// Validate checks a presented token and returns its info. It returns
// ErrSessionNotFound / ErrSessionExpired / ErrSessionRevoked (or a
// wrapped database error).
func (m *Manager) Validate(ctx context.Context, token string) (*Info, error) {
	if token == "" {
		return nil, ErrSessionNotFound
	}
	var (
		sid, userID, expiresStr string
		revokedAt               sql.NullString
	)
	err := m.dbh.QueryRowContext(ctx,
		`SELECT id, user_id, expires_at, revoked_at
		   FROM sessions WHERE token_hash = ?`, TokenHash(token),
	).Scan(&sid, &userID, &expiresStr, &revokedAt)
	if err == sql.ErrNoRows {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("validate session: %w", err)
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresStr)
	if err != nil {
		return nil, ErrSessionNotFound // corrupt row: fail closed
	}
	if revokedAt.Valid {
		return nil, ErrSessionRevoked
	}
	if m.nowf().After(expiresAt) {
		return nil, ErrSessionExpired
	}
	return &Info{SessionID: sid, UserID: userID, ExpiresAt: expiresAt, Token: token}, nil
}

// Revoke revokes one session (idempotent).
func (m *Manager) Revoke(ctx context.Context, sessionID string) error {
	_, err := m.dbh.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ?
		  WHERE id = ? AND (revoked_at IS NULL OR revoked_at = '')`,
		m.nowf().UTC().Format(time.RFC3339), sessionID)
	return err
}

// RevokeAllForUser revokes every live session of a user. Returns the
// number of sessions revoked.
func (m *Manager) RevokeAllForUser(ctx context.Context, userID string) (int, error) {
	res, err := m.dbh.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ?
		  WHERE user_id = ? AND (revoked_at IS NULL OR revoked_at = '')`,
		m.nowf().UTC().Format(time.RFC3339), userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ActiveCount reports live (unexpired, unrevoked) sessions for a user.
func (m *Manager) ActiveCount(ctx context.Context, userID string) (int, error) {
	var n int
	err := m.dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions
		  WHERE user_id = ? AND (revoked_at IS NULL OR revoked_at = '') AND expires_at > ?`,
		userID, m.nowf().UTC().Format(time.RFC3339)).Scan(&n)
	return n, err
}

// SetNowFunc is a test seam (replaces the clock).
func (m *Manager) SetNowFunc(f func() time.Time) { m.nowf = f }
