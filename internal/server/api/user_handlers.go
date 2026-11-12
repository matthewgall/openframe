package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/matthewgall/openframe/internal/server/auth"
	"github.com/matthewgall/openframe/internal/server/users"
)

// handleListUsers (admin): GET /api/v1/users?active=1
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active") == "1"
	list, err := s.userStore.List(r.Context(), activeOnly)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	if list == nil {
		list = []users.User{}
	}
	out := make([]map[string]any, len(list))
	for i := range list {
		out[i] = publicUser(&list[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

type createUserRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// handleCreateUser (admin): POST /api/v1/users
// Returns a temporary password, shown exactly once (administrator-
// controlled recovery flow, spec §5).
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Role == "" {
		req.Role = users.RoleMember
	}
	tmp, err := auth.TemporaryPassword()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not generate password")
		return
	}
	u, err := s.userStore.Create(r.Context(), req.Email, req.DisplayName, req.Role, tmp)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "user.created", "user", u.ID, map[string]string{"email": u.Email, "role": u.Role})
	writeJSON(w, http.StatusCreated, map[string]any{
		"user":               publicUser(u),
		"temporary_password": tmp,
		"note":               "share this password with the user; it must be changed at first login",
	})
}

type updateUserRequest struct {
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Deactivated *bool  `json:"deactivated"`
}

// handleUpdateUser (admin): PATCH /api/v1/users/{id}
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	var req updateUserRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	actor, _ := UserFrom(r.Context())

	// Role/display updates.
	if req.DisplayName != "" || req.Role != "" {
		if _, err := s.userStore.Update(r.Context(), id, req.DisplayName, req.Role); err != nil {
			s.writeDomainError(w, err)
			return
		}
		s.audit(r, "user.updated", "user", id, map[string]string{"actor": deref(actor)})
	}
	// Deactivation / reactivation.
	if req.Deactivated != nil {
		u, err := s.userStore.SetDeactivated(r.Context(), id, *req.Deactivated)
		if err != nil {
			s.writeDomainError(w, err)
			return
		}
		if *req.Deactivated {
			// Kill all live sessions of a deactivated user immediately.
			if n, err := s.sessionMgr.RevokeAllForUser(r.Context(), id); err == nil && n > 0 {
				s.audit(r, "user.sessions.revoked", "user", id, map[string]int{"count": n})
			}
		}
		s.audit(r, "user.deactivated", "user", id, map[string]any{"deactivated": *req.Deactivated, "email": u.Email})
	}

	u, err := s.userStore.Get(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(u)})
}

// handleResetPassword (admin): POST /api/v1/users/{id}/password-reset
// Issues a new temporary password (spec §5: password reset through
// administrator-controlled recovery).
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	tmp, err := auth.TemporaryPassword()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not generate password")
		return
	}
	if err := s.userStore.SetPassword(r.Context(), id, tmp); err != nil {
		s.writeDomainError(w, err)
		return
	}
	// The old password is gone: revoke all sessions of that user.
	if n, err := s.sessionMgr.RevokeAllForUser(r.Context(), id); err == nil && n > 0 {
		s.audit(r, "user.sessions.revoked", "user", id, map[string]int{"count": n})
	}
	s.audit(r, "user.password_reset", "user", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"temporary_password": tmp,
		"note":               "all sessions for this user were revoked",
	})
}

// handleRevokeSessions (admin): POST /api/v1/users/{id}/sessions/revoke
func (s *Server) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	}
	n, err := s.sessionMgr.RevokeAllForUser(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "user.sessions.revoked", "user", id, map[string]int{"count": n})
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}
