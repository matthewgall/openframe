package pairing

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/id"
	"github.com/matthewgall/openframe/internal/server/devices"
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
	return dbh
}

func newFrameID(t *testing.T) string {
	t.Helper()
	idv, err := id.NewUUID()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return idv
}

func TestPairingCreateAndApprove(t *testing.T) {
	ctx := context.Background()
	dbh := newTestDB(t)
	st := NewStore(dbh)
	fid := newFrameID(t)

	created, err := st.Create(ctx, fid, "Living Room", "10.0.0.5")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(created.Code) != codeLen {
		t.Fatalf("code length = %d, want %d", len(created.Code), codeLen)
	}
	for _, c := range created.Code {
		if !strings.ContainsRune(codeAlphabet, c) {
			t.Fatalf("code %q contains ambiguous rune %q", created.Code, c)
		}
	}

	// Status before approval.
	status, devID, _, err := st.Status(ctx, created.ID)
	if err != nil || status != StatusPending || devID != "" {
		t.Fatalf("status = (%q, %q, %v)", status, devID, err)
	}

	// Wrong code is rejected.
	if _, _, err := st.Approve(ctx, created.ID, "22222222"); err != ErrWrongCode {
		t.Fatalf("wrong-code approve = %v, want ErrWrongCode", err)
	}
	// The challenge is still pending after a failed guess.
	if status, _, _, _ = st.Status(ctx, created.ID); status != StatusPending {
		t.Fatalf("status after bad guess = %q, want pending", status)
	}

	// Correct code: atomic device + credential.
	approvedID, token, err := st.Approve(ctx, created.ID, created.Code)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approvedID != fid {
		t.Fatalf("approved device = %q, want frame identity %q", approvedID, fid)
	}
	if !strings.HasPrefix(token, "ofdv1_") {
		t.Fatalf("token missing prefix: %q", token[:6])
	}

	dstore := devices.NewStore(dbh)
	if err := dstore.VerifyCredential(ctx, fid, token); err != nil {
		t.Fatalf("issued credential does not verify: %v", err)
	}

	status, devID, _, err = st.Status(ctx, created.ID)
	if err != nil || status != StatusApproved || devID != fid {
		t.Fatalf("status = (%q, %q, %v)", status, devID, err)
	}

	// Single-use: a second approval fails.
	if _, _, err := st.Approve(ctx, created.ID, created.Code); err != ErrNotPending {
		t.Fatalf("second approve = %v, want ErrNotPending", err)
	}
}

func TestPairingCredentialOneTimeClaim(t *testing.T) {
	ctx := context.Background()
	dbh := newTestDB(t)
	st := NewStore(dbh)
	fid := newFrameID(t)

	created, err := st.Create(ctx, fid, "Claim", "10.0.0.6")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, token, err := st.Approve(ctx, created.ID, created.Code)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// No credential before the first post-approval poll.
	if _, _, cred, err := st.Status(ctx, created.ID); err != nil || cred != token {
		t.Fatalf("first claim = (%q, %v), want the issued token", cred, err)
	}
	// The claim is one-time.
	if _, _, cred, err := st.Status(ctx, created.ID); err != nil || cred != "" {
		t.Fatalf("second claim = (%q, %v), want empty", cred, err)
	}
	// The raw token is gone from the table.
	var n int
	if err := dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pairing_challenges WHERE credential IS NOT NULL").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("raw credential still at rest in %d rows", n)
	}
}

func TestPairingRejectAndNotFound(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newTestDB(t))
	fid := newFrameID(t)

	created, err := st.Create(ctx, fid, "Hall", "10.0.0.9")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Reject(ctx, created.ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if status, _, _, err := st.Status(ctx, created.ID); err != nil || status != StatusRejected {
		t.Fatalf("status = (%q, %v)", status, err)
	}
	if err := st.Reject(ctx, created.ID); err == nil {
		t.Fatal("double reject should fail")
	}
	if _, _, _, err := st.Status(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("missing status = %v, want ErrNotFound", err)
	}
}

func TestPairingExpiry(t *testing.T) {
	ctx := context.Background()
	dbh := newTestDB(t)
	st := NewStore(dbh)
	fid := newFrameID(t)

	created, err := st.Create(ctx, fid, "Desk", "10.0.0.1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force expiry into the past.
	if _, err := dbh.ExecContext(ctx,
		`UPDATE pairing_challenges SET expires_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), created.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if status, _, _, err := st.Status(ctx, created.ID); err != nil || status != StatusExpired {
		t.Fatalf("status = (%q, %v)", status, err)
	}
	if _, _, err := st.Approve(ctx, created.ID, created.Code); err != ErrNotPending {
		t.Fatalf("approve expired = %v, want ErrNotPending", err)
	}
}

func TestPairingRateLimit(t *testing.T) {
	ctx := context.Background()
	st := NewStore(newTestDB(t))

	for i := 0; i < MaxPendingPerIP; i++ {
		if _, err := st.Create(ctx, newFrameID(t), "F", "10.1.1.1"); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := st.Create(ctx, newFrameID(t), "F", "10.1.1.1"); err != ErrRateLimit {
		t.Fatalf("create past limit = %v, want ErrRateLimit", err)
	}
	// A different address is unaffected.
	if _, err := st.Create(ctx, newFrameID(t), "F", "10.1.1.2"); err != nil {
		t.Fatalf("other ip create: %v", err)
	}
}

func TestPairingRejectsRegisteredDevice(t *testing.T) {
	ctx := context.Background()
	dbh := newTestDB(t)
	st := NewStore(dbh)
	fid := newFrameID(t)

	// Register the identity manually first.
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO devices (id, name, created_at) VALUES (?, 'Manual', ?)`,
		fid, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, fid, "Manual", "10.0.0.2"); err != ErrExists {
		t.Fatalf("create for registered device = %v, want ErrExists", err)
	}
}

func TestPairingListPending(t *testing.T) {
	ctx := context.Background()
	dbh := newTestDB(t)
	st := NewStore(dbh)

	a, _ := st.Create(ctx, newFrameID(t), "A", "10.0.0.3")
	b, _ := st.Create(ctx, newFrameID(t), "B", "10.0.0.4")

	pending, err := st.ListPending(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(pending))
	}
	for _, p := range pending {
		if p.Status != StatusPending {
			t.Fatalf("pending entry status = %q", p.Status)
		}
	}
	if pending[0].DeviceName != "A" || pending[1].DeviceName != "B" {
		t.Fatalf("ordering: %q then %q", pending[0].DeviceName, pending[1].DeviceName)
	}

	// Consumed challenges disappear from the list.
	if _, _, err := st.Approve(ctx, a.ID, a.Code); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, _, err := st.Approve(ctx, b.ID, b.Code); err != nil {
		t.Fatalf("approve: %v", err)
	}
	pending, err = st.ListPending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after approvals = %d (%v), want 0", len(pending), err)
	}
}
