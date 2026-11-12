package api

import (
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
