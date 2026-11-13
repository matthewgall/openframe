package id

import (
	"regexp"
	"strings"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewUUIDFormat(t *testing.T) {
	for i := 0; i < 100; i++ {
		u, err := NewUUID()
		if err != nil {
			t.Fatal(err)
		}
		if !uuidRe.MatchString(u) {
			t.Fatalf("bad uuid %q", u)
		}
	}
}

func TestNewUUIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		u, _ := NewUUID()
		if seen[u] {
			t.Fatalf("duplicate uuid %q", u)
		}
		seen[u] = true
	}
}

func TestIsValidUUID(t *testing.T) {
	valid, _ := NewUUID()
	if !IsValidUUID(valid) {
		t.Fatalf("generated uuid %q rejected", valid)
	}
	for _, bad := range []string{
		"", "not-a-uuid", strings.ToUpper(valid),
		valid + "x", valid[:35], strings.Replace(valid, "-", "", 1),
		strings.Replace(valid, "a", "g", 1),
	} {
		if IsValidUUID(bad) {
			t.Fatalf("%q accepted as uuid", bad)
		}
	}
}
