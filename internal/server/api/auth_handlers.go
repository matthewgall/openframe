package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/server/auth"
	"github.com/matthewgall/openframe/internal/server/users"
)

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "email and password are required")
		return
	}

	ip := clientIP(r)
	// Two independent throttles: per IP (distributed guessing) and per
	// account (credential stuffing against one family member).
	if !s.loginLimiter.Allow("ip:"+ip, s.cfg.Server.LoginRateLimitPerMin) {
		s.auditRec.Action(r.Context(), "", "login.rate_limited", "user", req.Email, ip, nil)
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts, try again later")
		return
	}
	if !s.loginLimiter.Allow("email:"+strings.ToLower(req.Email), 10) {
		s.auditRec.Action(r.Context(), "", "login.rate_limited", "user", req.Email, ip, nil)
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts, try again later")
		return
	}

	u, err := s.userStore.GetByEmail(r.Context(), req.Email)
	if err != nil {
		// Uniform error (do not reveal whether the account exists);
		// still run argon2 to keep timing even.
		_ = auth.VerifyPassword(dummyHashCache, req.Password)
		s.auditRec.Action(r.Context(), "", "login.failed", "user", req.Email, ip, map[string]string{"reason": "bad_credentials"})
		writeError(w, http.StatusUnauthorized, "bad_credentials", "invalid email or password")
		return
	}
	if u.DeactivatedAt != nil {
		s.auditRec.Action(r.Context(), "", "login.failed", "user", u.ID, ip, map[string]string{"reason": "deactivated"})
		writeError(w, http.StatusUnauthorized, "bad_credentials", "invalid email or password")
		return
	}
	ok, err := s.userStore.VerifyPassword(r.Context(), u.ID, req.Password)
	if err != nil || !ok {
		s.auditRec.Action(r.Context(), u.ID, "login.failed", "user", u.ID, ip, map[string]string{"reason": "bad_credentials"})
		writeError(w, http.StatusUnauthorized, "bad_credentials", "invalid email or password")
		return
	}

	// 2FA (TOTP): a correct password is not enough when the user has
	// enabled two-factor. Hand the client a single-use challenge id;
	// the session is created only after /auth/mfa verifies a code.
	if _, enforced, err := s.userStore.TOTPEnforced(r.Context(), u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not check two-factor state")
		return
	} else if enforced {
		mfaID, err := s.newMFAChallenge(u.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "could not start two-factor check")
			return
		}
		s.auditRec.Action(r.Context(), u.ID, "login.mfa_required", "user", u.ID, ip, nil)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"mfa_required": true,
			"mfa_id":       mfaID,
		})
		return
	}

	sess, err := s.sessionMgr.Create(r.Context(), u.ID, truncate(r.UserAgent(), 200), ip)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not create session")
		return
	}
	s.setSessionCookie(w, sess.Token, sess.ExpiresAt)
	s.loginLimiter.Reset("email:" + strings.ToLower(req.Email))
	s.auditRec.Action(r.Context(), u.ID, "login.succeeded", "user", u.ID, ip, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       publicUser(u),
		"csrf_token": auth.DeriveCSRFToken(sess.Token),
		"expires_at": sess.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleCSRF(w http.ResponseWriter, r *http.Request) {
	sess, _ := SessionFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": auth.DeriveCSRFToken(sess.Token)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess, _ := SessionFrom(r.Context())
	u, _ := UserFrom(r.Context())
	if sess != nil {
		_ = s.sessionMgr.Revoke(r.Context(), sess.SessionID)
	}
	s.clearSessionCookie(w)
	s.audit(r, "logout", "user", deref(u), nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	sess, _ := SessionFrom(r.Context())
	resp := map[string]any{
		"user": map[string]any{
			"id":           u.ID,
			"email":        u.Email,
			"display_name": u.DisplayName,
			"role":         u.Role,
		},
	}
	if sess != nil {
		resp["session"] = map[string]any{
			"id":         sess.SessionID,
			"expires_at": sess.ExpiresAt.UTC().Format(time.RFC3339),
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if len(req.NewPassword) < auth.MinPasswordLength {
		writeError(w, http.StatusBadRequest, "password_too_short",
			"password must be at least "+strconv.Itoa(auth.MinPasswordLength)+" characters")
		return
	}
	u, _ := UserFrom(r.Context())
	ok, err := s.userStore.VerifyPassword(r.Context(), u.ID, req.CurrentPassword)
	if err != nil || !ok {
		writeError(w, http.StatusUnauthorized, "bad_credentials", "current password is incorrect")
		return
	}
	if err := s.userStore.SetPassword(r.Context(), u.ID, req.NewPassword); err != nil {
		status, code, msg := domainError(err)
		writeError(w, status, code, msg)
		return
	}
	s.audit(r, "password.changed", "user", u.ID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "changed"})
}

type updateProfileRequest struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

// handleUpdateProfile: PATCH /api/v1/auth/profile — a user updates
// their own display name and/or email. Empty fields are left as-is.
func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	var req updateProfileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" && strings.TrimSpace(req.Email) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "nothing to update")
		return
	}
	u, _ := UserFrom(r.Context())
	updated, err := s.userStore.UpdateProfile(r.Context(), u.ID, req.DisplayName, req.Email)
	if err != nil {
		status, code, msg := domainError(err)
		writeError(w, status, code, msg)
		return
	}
	old := u.Email
	if old != updated.Email {
		s.audit(r, "profile.email_changed", "user", u.ID, map[string]string{"from": old, "to": updated.Email})
	} else {
		s.audit(r, "profile.updated", "user", u.ID, nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(updated)})
}

// publicUser is the JSON-safe user shape (never includes the hash).
func publicUser(u *users.User) map[string]any {
	m := map[string]any{
		"id":           u.ID,
		"email":        u.Email,
		"display_name": u.DisplayName,
		"role":         u.Role,
		"created_at":   u.CreatedAt.UTC().Format(time.RFC3339),
	}
	if u.DeactivatedAt != nil {
		m["deactivated_at"] = u.DeactivatedAt.UTC().Format(time.RFC3339)
	}
	return m
}

func deref(u *AuthUser) string {
	if u == nil {
		return ""
	}
	return u.ID
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// dummyHashCache makes failed-username logins take the same time as
// real verifications (constant-timing hygiene).
var dummyHashCache = mustDummyHash()

func mustDummyHash() string {
	h, err := auth.HashPassword("openframe-timing-equalizer")
	if err != nil {
		panic(err)
	}
	return h
}

// ---------------------------------------------------------------------
// 2FA (TOTP) — the MFA step of login.

// mfaChallenge is one pending 2FA step. Single-use, short-TTL, and
// attempt-capped: after the cap the challenge is destroyed, so a
// stolen mfa_id yields at most MFAAttemptsMax code guesses (each
// 6-digit or 11-char — both effectively unguessable, the cap is
// defence-in-depth plus audit noise control).
type mfaChallenge struct {
	userID    string
	expiresAt time.Time
	attempts  int
}

const (
	mfaTTL         = 5 * time.Minute
	mfaAttemptsMax = 10
)

func (s *Server) newMFAChallenge(userID string) (string, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	mfaID := hex.EncodeToString(id)
	s.mfaMu.Lock()
	// Opportunistic GC: drop expired challenges on the way in.
	for k, ch := range s.mfaChallenges {
		if time.Now().After(ch.expiresAt) {
			delete(s.mfaChallenges, k)
		}
	}
	s.mfaChallenges[mfaID] = &mfaChallenge{userID: userID, expiresAt: time.Now().Add(mfaTTL)}
	s.mfaMu.Unlock()
	return mfaID, nil
}

// POST /api/v1/auth/mfa  (public: no session exists yet)
// Body: {"mfa_id": "...", "code": "123456" | "ABCDE-FGHIJ"}
// 200: session cookie + {user, csrf_token, expires_at} (login shape)
// 401: invalid challenge, or code did not verify
// 429: attempt cap exceeded (challenge destroyed)
func (s *Server) handleMFA(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MFAID string `json:"mfa_id"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	req.MFAID = strings.TrimSpace(req.MFAID)
	req.Code = strings.TrimSpace(req.Code)
	if req.MFAID == "" || req.Code == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "mfa_id and code are required")
		return
	}
	ip := clientIP(r)

	s.mfaMu.Lock()
	ch, ok := s.mfaChallenges[req.MFAID]
	if ok {
		ch.attempts++
	}
	exceeded := ok && ch.attempts > mfaAttemptsMax
	if ok && (time.Now().After(ch.expiresAt) || exceeded) {
		delete(s.mfaChallenges, req.MFAID)
	}
	s.mfaMu.Unlock()

	auditUser := func() string {
		if ok {
			return ch.userID
		}
		return ""
	}

	if !ok {
		// Uniform error: expired/unknown/already-consumed are the same.
		s.auditRec.Action(r.Context(), auditUser(), "login.mfa_failed", "user", auditUser(), ip, map[string]string{"reason": "invalid_challenge"})
		writeError(w, http.StatusUnauthorized, "invalid_challenge", "this sign-in step has expired, sign in again")
		return
	}
	if exceeded {
		s.auditRec.Action(r.Context(), ch.userID, "login.mfa_locked", "user", ch.userID, ip, map[string]string{"reason": "attempt_cap"})
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many two-factor attempts, sign in again")
		return
	}

	// The secret is only fetched after the challenge checks pass, so a
	// garbage mfa_id does not even touch the user's 2FA row.
	secret, enforced, err := s.userStore.TOTPEnforced(r.Context(), ch.userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not check two-factor state")
		return
	}
	var verified bool
	var via string
	switch {
	case enforced && len(req.Code) == auth.TOTPDigits && auth.IsRecoveryCodeShape(req.Code) == false:
		verified = auth.VerifyTOTP(secret, req.Code, time.Now())
		via = "totp"
	case auth.IsRecoveryCodeShape(req.Code):
		verified, err = s.userStore.RedeemRecoveryCode(r.Context(), ch.userID, req.Code)
		if err == nil {
			via = "recovery"
		}
	default:
		writeError(w, http.StatusUnauthorized, "bad_code", "enter the 6-digit code from your authenticator, or a recovery code")
		return
	}
	if !verified {
		s.auditRec.Action(r.Context(), ch.userID, "login.mfa_failed", "user", ch.userID, ip, map[string]string{"reason": "bad_code", "via": via})
		writeError(w, http.StatusUnauthorized, "bad_code", "the code did not verify, try again")
		return
	}

	// Consume the challenge (single-use) before creating the session.
	s.mfaMu.Lock()
	delete(s.mfaChallenges, req.MFAID)
	s.mfaMu.Unlock()

	u, err := s.userStore.Get(r.Context(), ch.userID)
	if err != nil || u.DeactivatedAt != nil {
		writeError(w, http.StatusUnauthorized, "invalid_session", "sign in required")
		return
	}
	sess, err := s.sessionMgr.Create(r.Context(), u.ID, truncate(r.UserAgent(), 200), ip)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not create session")
		return
	}
	s.setSessionCookie(w, sess.Token, sess.ExpiresAt)
	s.auditRec.Action(r.Context(), u.ID, "login.mfa_succeeded", "user", u.ID, ip, map[string]string{"via": via})
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       publicUser(u),
		"csrf_token": auth.DeriveCSRFToken(sess.Token),
		"expires_at": sess.ExpiresAt.UTC().Format(time.RFC3339),
	})
}
