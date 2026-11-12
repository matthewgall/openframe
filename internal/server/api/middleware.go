package api

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"

	"github.com/matthewgall/openframe/internal/server/auth"
	"github.com/matthewgall/openframe/internal/server/users"
)

type ctxKey int

const (
	userCtxKey ctxKey = iota
	sessionCtxKey
)

// AuthUser is the validated request identity.
type AuthUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// IsAdmin reports whether the user holds the admin role.
func (u *AuthUser) IsAdmin() bool { return u != nil && u.Role == users.RoleAdmin }

// UserFrom returns the authenticated user for the request context.
func UserFrom(ctx context.Context) (*AuthUser, bool) {
	u, ok := ctx.Value(userCtxKey).(*AuthUser)
	return u, ok
}

// SessionFrom returns the session info (token, expiry) for the context.
func SessionFrom(ctx context.Context) (*auth.Info, bool) {
	s, ok := ctx.Value(sessionCtxKey).(*auth.Info)
	return s, ok
}

func clientIP(r *http.Request) string {
	// Honour X-Forwarded-For only when present (behind a trusted proxy
	// per deployment); otherwise use the socket peer. The value is used
	// for rate limiting and audit only, never for authorization.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requireAuth authenticates the request: session cookie -> user row
// (must be active). It injects AuthUser + session into the context.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.sessionCookie(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		sess, err := s.sessionMgr.Validate(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid_session", "sign in required")
			return
		}
		u, err := s.userStore.Get(r.Context(), sess.UserID)
		if err != nil || u.DeactivatedAt != nil {
			writeError(w, http.StatusUnauthorized, "invalid_session", "sign in required")
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, &AuthUser{
			ID:          u.ID,
			Email:       u.Email,
			DisplayName: u.DisplayName,
			Role:        u.Role,
		})
		ctx = context.WithValue(ctx, sessionCtxKey, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAdmin gates admin-only routes.
func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		if !u.IsAdmin() {
			writeError(w, http.StatusForbidden, "admin_required", "administrator role required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrfProtect enforces the synchronizer token on all mutating
// requests. Login is exempt (no session exists yet); the SameSite=Lax
// cookie plus login rate limiting bound the residual risk (T-09).
func (s *Server) csrfProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == protocolAuthLoginPath {
			next.ServeHTTP(w, r)
			return
		}
		sess, ok := SessionFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		want := auth.DeriveCSRFToken(sess.Token)
		got := r.Header.Get("X-CSRF-Token")
		if subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
			writeError(w, http.StatusForbidden, "csrf_mismatch", "CSRF token missing or invalid")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sessionCookie reads the session cookie.
func (s *Server) sessionCookie(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
