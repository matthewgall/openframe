// Package api implements the versioned REST API (spec §5/§6, M2
// surface; conventions in docs/api/overview.md).
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/matthewgall/openframe/internal/server/albums"
	"github.com/matthewgall/openframe/internal/server/auth"
	"github.com/matthewgall/openframe/internal/server/photos"
	"github.com/matthewgall/openframe/internal/server/users"
)

// errorBody is the stable machine-readable error shape (spec §19).
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody{Error: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// isBodyTooLarge reports whether the error came from an http.MaxBytes
// reader (the reader's error is not exposed as a typed sentinel by
// every Go version, so match the stable message text).
func isBodyTooLarge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "http: request body too large")
}

// writeDomainError maps a domain sentinel to a response.
func (s *Server) writeDomainError(w http.ResponseWriter, err error) {
	status, code, msg := domainError(err)
	if status == http.StatusInternalServerError {
		s.log.Error("api internal error", "error", err)
	}
	writeError(w, status, code, msg)
}

// domainError maps a package-level sentinel to (status, code, message).
func domainError(err error) (int, string, string) {
	switch {
	case errors.Is(err, photos.ErrUnsupportedFormat),
		errors.Is(err, photos.ErrWebPNotLossy):
		return http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error()
	case errors.Is(err, photos.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds the upload size limit"
	case errors.Is(err, photos.ErrTooManyFiles):
		return http.StatusRequestEntityTooLarge, "too_many_files", "too many files in one upload"
	case errors.Is(err, photos.ErrDimensionsExceed):
		return http.StatusRequestEntityTooLarge, "dimensions_exceed_limit", "image exceeds the maximum decode dimensions"
	case errors.Is(err, photos.ErrDecodeFailed):
		return http.StatusBadRequest, "invalid_image", "file is not a valid image"
	case errors.Is(err, photos.ErrNotFound),
		errors.Is(err, albums.ErrNotFound),
		errors.Is(err, users.ErrNotFound),
		errors.Is(err, auth.ErrSessionNotFound):
		return http.StatusNotFound, "not_found", "resource not found"
	case errors.Is(err, users.ErrEmailTaken):
		return http.StatusConflict, "email_taken", "email is already registered"
	case errors.Is(err, users.ErrInvalidRole):
		return http.StatusBadRequest, "invalid_role", "role must be admin or member"
	case errors.Is(err, users.ErrNoActiveUsers):
		return http.StatusConflict, "last_active_admin", "cannot deactivate the last active administrator"
	case errors.Is(err, auth.ErrSessionExpired):
		return http.StatusUnauthorized, "session_expired", "session expired, sign in again"
	case errors.Is(err, auth.ErrSessionRevoked):
		return http.StatusUnauthorized, "session_revoked", "session was revoked, sign in again"
	}
	return http.StatusInternalServerError, "internal", "internal server error"
}
