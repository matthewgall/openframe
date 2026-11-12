// Package users implements the user domain (spec §5: multiple family
// members, administrator and member roles, administrator-controlled
// password recovery).
package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/id"
	"github.com/matthewgall/openframe/internal/server/auth"
)

// Roles.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Errors surfaced to the API layer.
var (
	ErrNotFound      = errors.New("users: user not found")
	ErrEmailTaken    = errors.New("users: email already registered")
	ErrInvalidRole   = errors.New("users: invalid role")
	ErrNoActiveUsers = errors.New("users: cannot remove the last active user")
)

// User is the domain record (no password material).
type User struct {
	ID            string     `json:"id"`
	Email         string     `json:"email"`
	DisplayName   string     `json:"display_name"`
	Role          string     `json:"role"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
}

// Store performs user operations against SQLite.
type Store struct{ dbh *sql.DB }

func NewStore(dbh *sql.DB) *Store { return &Store{dbh: dbh} }

// Create inserts a new user with a hashed password and returns the
// user. email is normalised (lowercase); duplicates are ErrEmailTaken.
func (s *Store) Create(ctx context.Context, email, displayName, role, password string) (*User, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return nil, errors.New("users: empty email")
	}
	if displayName == "" {
		displayName = email
	}
	if role != RoleAdmin && role != RoleMember {
		return nil, ErrInvalidRole
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	uid, err := id.NewUUID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.dbh.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, role, password_hash, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uid, email, displayName, role, hash, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	return &User{ID: uid, Email: email, DisplayName: displayName, Role: role,
		CreatedAt: parseTS(now), UpdatedAt: parseTS(now)}, nil
}

// Get fetches one user (active or deactivated).
func (s *Store) Get(ctx context.Context, id string) (*User, error) {
	return s.get(ctx, "id", id)
}

// GetByEmail fetches one user by email (any state).
func (s *Store) GetByEmail(ctx context.Context, email string) (*User, error) {
	return s.get(ctx, "email", NormalizeEmail(email))
}

func (s *Store) get(ctx context.Context, col, val string) (*User, error) {
	var u User
	var deactivated sql.NullString
	var created, updated string
	err := s.dbh.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id, email, display_name, role, created_at, updated_at, deactivated_at
		           FROM users WHERE %s = ?`, col), val).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &created, &updated, &deactivated)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt = parseTS(created)
	u.UpdatedAt = parseTS(updated)
	if deactivated.Valid && deactivated.String != "" {
		t := parseTS(deactivated.String)
		u.DeactivatedAt = &t
	}
	return &u, nil
}

// List returns all users (newest first) with an optional active-only
// filter.
func (s *Store) List(ctx context.Context, activeOnly bool) ([]User, error) {
	q := `SELECT id, email, display_name, role, created_at, updated_at, deactivated_at
	       FROM users`
	if activeOnly {
		q += ` WHERE deactivated_at IS NULL`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.dbh.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var deactivated sql.NullString
		var created, updated string
		if err := rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &created, &updated, &deactivated); err != nil {
			return nil, err
		}
		u.CreatedAt = parseTS(created)
		u.UpdatedAt = parseTS(updated)
		if deactivated.Valid && deactivated.String != "" {
			t := parseTS(deactivated.String)
			u.DeactivatedAt = &t
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Update mutates display name and/or role. role may be "" (unchanged);
// name may be "" (unchanged).
func (s *Store) Update(ctx context.Context, id, displayName, role string) (*User, error) {
	u, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if role != "" && role != RoleAdmin && role != RoleMember {
		return nil, ErrInvalidRole
	}
	// Never allow demoting the last active admin (server lockout).
	if role == RoleMember && u.Role == RoleAdmin && u.DeactivatedAt == nil {
		n, err := s.countActiveAdmins(ctx)
		if err != nil {
			return nil, err
		}
		if n <= 1 {
			return nil, ErrNoActiveUsers
		}
	}
	if _, err := s.dbh.ExecContext(ctx,
		`UPDATE users SET display_name = ?, role = ?, updated_at = ?
		  WHERE id = ?`,
		nameOr(displayName, u.DisplayName),
		roleOr(role, u.Role),
		time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// SetPassword replaces the password (hashed).
func (s *Store) SetPassword(ctx context.Context, id, password string) error {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDeactivated activates/deactivates. Deactivated users cannot log
// in and have their sessions revoked by the caller.
func (s *Store) SetDeactivated(ctx context.Context, id string, deactivated bool) (*User, error) {
	u, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// Never allow deactivating the last active admin (lockout).
	if deactivated && u.Role == RoleAdmin {
		n, err := s.countActiveAdmins(ctx)
		if err != nil {
			return nil, err
		}
		if n <= 1 && u.DeactivatedAt == nil {
			return nil, ErrNoActiveUsers
		}
	}
	var val any
	now := time.Now().UTC().Format(time.RFC3339)
	if deactivated {
		val = now
	}
	if _, err := s.dbh.ExecContext(ctx,
		`UPDATE users SET deactivated_at = ?, updated_at = ? WHERE id = ?`,
		val, now, id); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// CountActive counts users that are not deactivated (first-run and
// capacity checks).
func (s *Store) CountActive(ctx context.Context) (int, error) {
	var n int
	err := s.dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE deactivated_at IS NULL`).Scan(&n)
	return n, err
}

func (s *Store) countActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = 'admin' AND deactivated_at IS NULL`).Scan(&n)
	return n, err
}

// VerifyPassword reports whether the user's password matches.
func (s *Store) VerifyPassword(ctx context.Context, id, password string) (bool, error) {
	var hash string
	err := s.dbh.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE id = ?`, id).Scan(&hash)
	if err == sql.ErrNoRows {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return auth.VerifyPassword(hash, password) == nil, nil
}

// NormalizeEmail lowercases and trims an address (SQLite email column
// is also COLLATE NOCASE, so lookups are robust either way).
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nameOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func roleOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// isUniqueViolation matches the SQLite unique-constraint message as
// emitted by modernc.org/sqlite (e.g. "SQL logic error: UNIQUE
// constraint failed: users.email").
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") && strings.Contains(msg, "users.email")
}
