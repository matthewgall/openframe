package api_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/matthewgall/openframe/internal/server/users"
)

// enable2FA turns on and confirms TOTP for the seeded admin; returns
// the secret and the one-time recovery codes.
func enable2FA(t *testing.T, env *m5Env) (secret string, recovery []string) {
	t.Helper()
	var out struct {
		Secret        string   `json:"secret"`
		OTPAuthURL    string   `json:"otpauth_url"`
		QRPNG         string   `json:"qr_png"`
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if code := env.admin.json(http.MethodPost, "/api/v1/users/me/totp/enable",
		map[string]string{"password": "wrong-password-1"}, &out); code != http.StatusUnauthorized {
		t.Fatalf("enable with bad password: %d, want 401", code)
	}
	if code := env.admin.json(http.MethodPost, "/api/v1/users/me/totp/enable",
		map[string]string{"password": "pw-admin-123"}, &out); code != http.StatusOK {
		t.Fatalf("enable: %d %v", code, out)
	}
	if out.Secret == "" || len(out.RecoveryCodes) != 10 || out.OTPAuthURL == "" || out.QRPNG == "" {
		t.Fatalf("enable response missing fields: %+v", out)
	}
	if code := env.admin.json(http.MethodPost, "/api/v1/users/me/totp/confirm",
		map[string]string{"code": "000000"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("confirm with bad code: %d, want 401", code)
	}
	live, err := totp.GenerateCode(out.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code := env.admin.json(http.MethodPost, "/api/v1/users/me/totp/confirm",
		map[string]string{"code": live}, nil); code != http.StatusOK {
		t.Fatalf("confirm: %d", code)
	}
	return out.Secret, out.RecoveryCodes
}

// freshLogin does a full login on a NEW cookie jar and returns the
// final response status plus any mfa_id from the first response.
func freshLogin(t *testing.T, env *m5Env, c *client) (int, string) {
	t.Helper()
	var first struct {
		MFARequired bool   `json:"mfa_required"`
		MFAID       string `json:"mfa_id"`
	}
	code := c.json(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": "admin@test", "password": "pw-admin-123"}, &first)
	if first.MFARequired {
		return code, first.MFAID
	}
	return code, ""
}

func TestTOTPLoginFlow(t *testing.T) {
	env := newM5Env(t)
	enable2FA(t, env)

	// A fresh session now requires the second factor.
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: env.base, http: &http.Client{Jar: jar}}
	code, mfaID := freshLogin(t, env, c)
	if code != http.StatusAccepted || mfaID == "" {
		t.Fatalf("login with 2FA: %d mfa_id=%q, want 202 + id", code, mfaID)
	}

	// Wrong codes are rejected; the challenge survives.
	if code := c.json(http.MethodPost, "/api/v1/auth/mfa",
		map[string]string{"mfa_id": mfaID, "code": "000000"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad code: %d, want 401", code)
	}

	// The live code completes the login and sets a session.
	var out struct {
		CSRFToken string `json:"csrf_token"`
		User      struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	code = c.json(http.MethodPost, "/api/v1/auth/mfa",
		map[string]string{"mfa_id": mfaID, "code": mustTOTP(t, env.dbh, "admin@test")}, &out)
	if code != http.StatusOK || out.CSRFToken == "" {
		t.Fatalf("mfa: %d", code)
	}
	c.csrf = out.CSRFToken
	if code := c.json(http.MethodGet, "/api/v1/auth/me", nil, nil); code != http.StatusOK {
		t.Fatalf("session after mfa: %d", code)
	}

	// The challenge is single-use: replaying it (even with a good
	// code) is refused.
	if code := c.json(http.MethodPost, "/api/v1/auth/mfa",
		map[string]string{"mfa_id": mfaID, "code": "123456"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("replayed challenge: %d, want 401", code)
	}
}

func TestTOTPRecoveryCode(t *testing.T) {
	env := newM5Env(t)
	_, recovery := enable2FA(t, env)

	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: env.base, http: &http.Client{Jar: jar}}
	_, mfaID := freshLogin(t, env, c)
	if mfaID == "" {
		t.Fatal("no mfa challenge")
	}
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	code := c.json(http.MethodPost, "/api/v1/auth/mfa",
		map[string]string{"mfa_id": mfaID, "code": recovery[0]}, &out)
	if code != http.StatusOK {
		t.Fatalf("recovery code login: %d", code)
	}

	// Single-use: the same code on the next sign-in must fail.
	jar2, _ := cookiejar.New(nil)
	c2 := &client{t: t, base: env.base, http: &http.Client{Jar: jar2}}
	_, mfaID2 := freshLogin(t, env, c2)
	if mfaID2 == "" {
		t.Fatal("no second mfa challenge")
	}
	if code := c2.json(http.MethodPost, "/api/v1/auth/mfa",
		map[string]string{"mfa_id": mfaID2, "code": recovery[0]}, nil); code != http.StatusUnauthorized {
		t.Fatalf("reused recovery code: %d, want 401", code)
	}

	// The remaining count dropped by one.
	var st struct {
		RecoveryRemaining int `json:"recovery_remaining"`
	}
	env.admin.json(http.MethodGet, "/api/v1/users/me/totp", nil, &st)
	if st.RecoveryRemaining != 9 {
		t.Fatalf("recovery_remaining = %d, want 9", st.RecoveryRemaining)
	}
}

func TestTOTPAttemptCap(t *testing.T) {
	env := newM5Env(t)
	enable2FA(t, env)

	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: env.base, http: &http.Client{Jar: jar}}
	_, mfaID := freshLogin(t, env, c)
	if mfaID == "" {
		t.Fatal("no mfa challenge")
	}
	for i := 0; i < 10; i++ {
		if code := c.json(http.MethodPost, "/api/v1/auth/mfa",
			map[string]string{"mfa_id": mfaID, "code": "000000"}, nil); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i+1, code)
		}
	}
	// One attempt over the cap: the challenge is destroyed.
	if code := c.json(http.MethodPost, "/api/v1/auth/mfa",
		map[string]string{"mfa_id": mfaID, "code": "000000"}, nil); code != http.StatusTooManyRequests {
		t.Fatalf("over cap: %d, want 429", code)
	}
	// Even the right code can no longer resurrect the challenge.
	if code := c.json(http.MethodPost, "/api/v1/auth/mfa",
		map[string]string{"mfa_id": mfaID, "code": mustTOTP(t, env.dbh, "admin@test")}, nil); code != http.StatusUnauthorized {
		t.Fatalf("destroyed challenge: %d, want 401", code)
	}
}

func TestTOTPDisable(t *testing.T) {
	env := newM5Env(t)
	enable2FA(t, env)

	if code := env.admin.json(http.MethodPost, "/api/v1/users/me/totp/disable",
		map[string]string{"password": "nope-nope-123"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("disable with bad password: %d, want 401", code)
	}
	if code := env.admin.json(http.MethodPost, "/api/v1/users/me/totp/disable",
		map[string]string{"password": "pw-admin-123"}, nil); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}

	// Login is plain again.
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: env.base, http: &http.Client{Jar: jar}}
	code, mfaID := freshLogin(t, env, c)
	if code != http.StatusOK || mfaID != "" {
		t.Fatalf("login after disable: %d mfa_id=%q, want plain 200", code, mfaID)
	}
	var st struct {
		Enabled bool `json:"enabled"`
	}
	env.admin.json(http.MethodGet, "/api/v1/users/me/totp", nil, &st)
	if st.Enabled {
		t.Fatal("status still enabled after disable")
	}
}

// mustTOTP reads the stored secret for the user and mints a live code
// (tests only; production never does this).
func mustTOTP(t *testing.T, dbh *sql.DB, email string) string {
	t.Helper()
	store := users.NewStore(dbh)
	u, err := store.GetByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	secret, ok, err := store.TOTPEnforced(context.Background(), u.ID)
	if err != nil || !ok {
		t.Fatalf("no enforced totp: %v %v", ok, err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return code
}
