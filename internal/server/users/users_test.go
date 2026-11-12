package users

import (
	"context"
	"errors"
	"testing"
)

func TestCreateGetByEmail(t *testing.T) {
	dbh := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()

	const plain = "s3cret-password"
	u, err := s.Create(ctx, "Admin@Example.com ", "Admin", RoleAdmin, plain)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.Email != "admin@example.com" {
		t.Fatalf("email not normalized: %s", u.Email)
	}

	got, err := s.GetByEmail(ctx, "ADMIN@example.COM")
	if err != nil {
		t.Fatalf("get by email (case-insensitive): %v", err)
	}
	if got.ID != u.ID {
		t.Fatal("user mismatch")
	}
}

func TestCreateDuplicateEmail(t *testing.T) {
	dbh := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()
	const plain = "s3cret-password"
	if _, err := s.Create(ctx, "a@example.com", "A", RoleMember, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "a@example.com", "B", RoleMember, plain); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("expected email_taken, got: %v", err)
	}
}

func TestRoleValidation(t *testing.T) {
	dbh := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()
	const plain = "s3cret-password"
	if _, err := s.Create(ctx, "a@example.com", "A", "superuser", plain); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("expected invalid role, got: %v", err)
	}
}

func TestVerifyPassword(t *testing.T) {
	dbh := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()
	const plain = "s3cret-password"
	u, _ := s.Create(ctx, "a@example.com", "A", RoleMember, plain)

	ok, err := s.VerifyPassword(ctx, u.ID, "s3cret-password")
	if err != nil || !ok {
		t.Fatalf("verify: ok=%v err=%v", ok, err)
	}
	ok, _ = s.VerifyPassword(ctx, u.ID, "wrong-password-99")
	if ok {
		t.Fatal("wrong password accepted")
	}
}

func TestLastActiveAdminProtection(t *testing.T) {
	dbh := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()
	const plain = "s3cret-password"
	admin, _ := s.Create(ctx, "admin@example.com", "Admin", RoleAdmin, plain)
	// Deactivating the only admin must fail.
	if _, err := s.SetDeactivated(ctx, admin.ID, true); err == nil {
		t.Fatal("deactivated the last active admin")
	}
	// With a second admin, deactivating one succeeds.
	other, _ := s.Create(ctx, "other@example.com", "Other", RoleAdmin, plain)
	if _, err := s.SetDeactivated(ctx, admin.ID, true); err != nil {
		t.Fatalf("deactivate with 2 admins: %v", err)
	}
	// ...but deactivating the remaining active admin must fail.
	if _, err := s.SetDeactivated(ctx, other.ID, true); err == nil {
		t.Fatal("deactivated the last active admin (second one)")
	}
	// Reactivating works.
	if _, err := s.SetDeactivated(ctx, admin.ID, false); err != nil {
		t.Fatal(err)
	}
	// Now with two active admins again, deactivating `other` succeeds.
	if _, err := s.SetDeactivated(ctx, other.ID, true); err != nil {
		t.Fatalf("deactivate other with admin active: %v", err)
	}
	// And `admin` is the last active admin again: deactivation fails.
	if _, err := s.SetDeactivated(ctx, admin.ID, true); err == nil {
		t.Fatal("deactivated the last active admin (again)")
	}
}

func TestRoleChangeCannotDemoteLastAdmin(t *testing.T) {
	dbh := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()
	const plain = "s3cret-password"
	admin, _ := s.Create(ctx, "admin@example.com", "Admin", RoleAdmin, plain)
	if _, err := s.Update(ctx, admin.ID, "", RoleMember); err == nil {
		t.Fatal("demoted the last active admin to member")
	}
}

func TestListActiveOnly(t *testing.T) {
	dbh := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()
	const plain = "s3cret-password"
	a, _ := s.Create(ctx, "a@example.com", "A", RoleMember, plain)
	s.Create(ctx, "b@example.com", "B", RoleMember, plain)
	_, _ = s.SetDeactivated(ctx, a.ID, true)

	all, err := s.List(ctx, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %d %v", len(all), err)
	}
	active, err := s.List(ctx, true)
	if err != nil || len(active) != 1 {
		t.Fatalf("list active: %d %v", len(active), err)
	}
}

func TestCountActive(t *testing.T) {
	dbh := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()
	const plain = "s3cret-password"
	if n, _ := s.CountActive(ctx); n != 0 {
		t.Fatalf("expected 0 active, got %d", n)
	}
	// A member (no last-admin lockout applies).
	u, _ := s.Create(ctx, "a@example.com", "A", RoleMember, plain)
	if n, _ := s.CountActive(ctx); n != 1 {
		t.Fatalf("expected 1 active, got %d", n)
	}
	if _, err := s.SetDeactivated(ctx, u.ID, true); err != nil {
		t.Fatalf("deactivate member: %v", err)
	}
	if n, _ := s.CountActive(ctx); n != 0 {
		t.Fatalf("expected 0 active after deactivation, got %d", n)
	}
}
