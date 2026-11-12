package auth

import (
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	phc, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$") {
		t.Fatalf("unexpected PHC format: %s", phc)
	}
	if err := VerifyPassword(phc, "correct horse battery staple"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := VerifyPassword(phc, "wrong password"); err == nil {
		t.Fatal("verify accepted wrong password")
	}
}

func TestHashUniquenessSalt(t *testing.T) {
	a, _ := HashPassword("same-password-123")
	b, _ := HashPassword("same-password-123")
	if a == b {
		t.Fatal("identical hashes for the same password (missing salt?)")
	}
}

func TestHashMinLength(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("accepted short password")
	}
}

func TestVerifyRejectsGarbagePHC(t *testing.T) {
	cases := []string{
		"",
		"$argon2id$v=19",
		"$argon2id$v=20$m=65536,t=3,p=4$abc$def", // wrong version
		"$bcrypt$whatever",
		"$argon2id$v=19$m=-5,t=3,p=4$abc$def", // nonsense params
	}
	for _, c := range cases {
		if err := VerifyPassword(c, "any-password-123"); err == nil {
			t.Fatalf("garbage PHC accepted: %q", c)
		}
	}
}

func TestTemporaryPassword(t *testing.T) {
	p, err := TemporaryPassword()
	if err != nil {
		t.Fatalf("temporary password: %v", err)
	}
	if len(p) != 24 {
		t.Fatalf("expected 24 chars (5 groups of 4), got %d: %s", len(p), p)
	}
	groups := strings.Split(p, "-")
	if len(groups) != 5 {
		t.Fatalf("expected 5 dash-separated groups, got %d", len(groups))
	}
	for _, g := range groups {
		if len(g) != 4 {
			t.Fatalf("bad group length: %q", g)
		}
	}
	// Only the declared unambiguous alphabet (no 0/O/1/I).
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for _, r := range p {
		if r == '-' {
			continue
		}
		if !strings.Contains(alphabet, string(r)) {
			t.Fatalf("character %q outside the declared alphabet in %q", r, p)
		}
	}
	q, _ := TemporaryPassword()
	if p == q {
		t.Fatal("two generated temporary passwords are identical")
	}
}

func TestDeriveCSRFToken(t *testing.T) {
	token, _ := NewToken()
	a := DeriveCSRFToken(token)
	b := DeriveCSRFToken(token)
	if a != b {
		t.Fatal("CSRF token is not deterministic for the same session token")
	}
	if a == token {
		t.Fatal("CSRF token equals the session token")
	}
	other, _ := NewToken()
	if DeriveCSRFToken(other) == a {
		t.Fatal("different sessions derived the same CSRF token")
	}
}

func TestTokenHash(t *testing.T) {
	tok, _ := NewToken()
	if len(TokenHash(tok)) != 64 {
		t.Fatal("TokenHash must be sha256 hex (64 chars)")
	}
}
