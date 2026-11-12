package auth

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/migrations"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbh, err := db.Open(ctx, t.TempDir()+"/test.db", false)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, role, password_hash, created_at, updated_at)
		 VALUES ('u1', 'a@example.com', 'A', 'member', 'x', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return dbh
}

func TestSessionCreateValidate(t *testing.T) {
	dbh := newTestDB(t)
	m := NewManager(dbh, time.Hour)
	sess, err := m.Create(context.Background(), "u1", "test-agent", "10.0.0.1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(sess.Token) < 32 {
		t.Fatal("session token too short")
	}
	got, err := m.Validate(context.Background(), sess.Token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.UserID != "u1" || got.Token != sess.Token {
		t.Fatalf("unexpected session: %+v", got)
	}
}

func TestSessionTokenNotStoredPlaintext(t *testing.T) {
	dbh := newTestDB(t)
	m := NewManager(dbh, time.Hour)
	sess, _ := m.Create(context.Background(), "u1", "", "")
	var n int
	if err := dbh.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, sess.Token).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("plaintext session token found in sessions table")
	}
}

func TestSessionExpiry(t *testing.T) {
	dbh := newTestDB(t)
	m := NewManager(dbh, time.Minute)
	now := time.Now().UTC()
	m.SetNowFunc(func() time.Time { return now })
	sess, _ := m.Create(context.Background(), "u1", "", "")
	// Advance past the TTL.
	m.SetNowFunc(func() time.Time { return now.Add(2 * time.Minute) })
	if _, err := m.Validate(context.Background(), sess.Token); err == nil {
		t.Fatal("expired session validated")
	}
}

func TestSessionRevocation(t *testing.T) {
	dbh := newTestDB(t)
	m := NewManager(dbh, time.Hour)
	sess, _ := m.Create(context.Background(), "u1", "", "")
	if err := m.Revoke(context.Background(), sess.SessionID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := m.Validate(context.Background(), sess.Token); err == nil {
		t.Fatal("revoked session validated")
	}
}

func TestSessionRevokeAllForUser(t *testing.T) {
	dbh := newTestDB(t)
	m := NewManager(dbh, time.Hour)
	s1, _ := m.Create(context.Background(), "u1", "", "")
	s2, _ := m.Create(context.Background(), "u1", "", "")
	n, err := m.RevokeAllForUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 revoked, got %d", n)
	}
	for _, s := range []*Info{s1, s2} {
		if _, err := m.Validate(context.Background(), s.Token); err == nil {
			t.Fatal("session still valid after RevokeAllForUser")
		}
	}
}
