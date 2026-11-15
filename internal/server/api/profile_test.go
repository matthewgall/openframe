package api_test

import (
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

func TestUpdateProfile(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "alice@example.com", "member", "alice-pass-12345")
	seedUser(t, dbh, "bob@example.com", "member", "bob-pass-12345")
	alice := newClient(t, ts.URL, "alice@example.com", "alice-pass-12345")

	var out struct {
		User map[string]any `json:"user"`
	}

	// Update display name only.
	if code := alice.json(http.MethodPatch, "/api/v1/auth/profile",
		map[string]string{"display_name": "Alice A."}, &out); code != http.StatusOK {
		t.Fatalf("update name: %d", code)
	}
	if out.User["display_name"] != "Alice A." {
		t.Fatalf("display_name = %v, want Alice A.", out.User["display_name"])
	}
	if out.User["email"] != "alice@example.com" {
		t.Fatalf("email changed unexpectedly: %v", out.User["email"])
	}

	// Update email only (normalised to lowercase).
	if code := alice.json(http.MethodPatch, "/api/v1/auth/profile",
		map[string]string{"email": "ALICE-NEW@Example.com"}, &out); code != http.StatusOK {
		t.Fatalf("update email: %d", code)
	}
	if out.User["email"] != "alice-new@example.com" {
		t.Fatalf("email = %v, want alice-new@example.com", out.User["email"])
	}

	// The old email can no longer authenticate; the new one can.
	if code := rawLogin(t, ts.URL, "alice@example.com", "alice-pass-12345"); code != http.StatusUnauthorized {
		t.Fatalf("old email login: expected 401, got %d", code)
	}
	if code := rawLogin(t, ts.URL, "alice-new@example.com", "alice-pass-12345"); code != http.StatusOK {
		t.Fatalf("new email login: expected 200, got %d", code)
	}

	// Conflict: taking Bob's address.
	if code := alice.json(http.MethodPatch, "/api/v1/auth/profile",
		map[string]string{"email": "bob@example.com"}, nil); code != http.StatusConflict {
		t.Fatalf("taken email: expected 409, got %d", code)
	}

	// Validation.
	if code := alice.json(http.MethodPatch, "/api/v1/auth/profile",
		map[string]string{"email": "not-an-email"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad email: expected 400, got %d", code)
	}
	if code := alice.json(http.MethodPatch, "/api/v1/auth/profile",
		map[string]string{}, nil); code != http.StatusBadRequest {
		t.Fatalf("empty body: expected 400, got %d", code)
	}
}

// rawLogin performs a login and returns the status code without failing
// the test (newClient fatals on any non-200, which is wrong for
// negative-path assertions).
func rawLogin(t *testing.T, base, email, password string) int {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar}
	resp, err := hc.Post(base+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
