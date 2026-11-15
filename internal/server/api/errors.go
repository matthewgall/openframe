// Package api implements the versioned REST API (spec §5/§6, M2
// surface; conventions in docs/api/overview.md).
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/matthewgall/openframe/internal/server/albums"
	"github.com/matthewgall/openframe/internal/server/auth"
	"github.com/matthewgall/openframe/internal/server/devices"
	"github.com/matthewgall/openframe/internal/server/pairing"
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
	json.NewEncoder(w).Encode(normalizeNilCollections(v))
}

// normalizeNilCollections replaces nil slices and maps with empty
// collections before encoding. A JSON null list field is a landmine
// for every consumer ("photos": null crashed the Deleted view, the
// album modal, …) — in this API a collection means "possibly empty",
// never "absent". It walks structs, maps and slices via reflection so
// every response shape is covered. Two kinds of value are opaque and
// pass through untouched: byte-string slices ([]byte, json.RawMessage
// — JSON encodes them base64 or verbatim, never as lists; latestResponse
// carries signed metadata that must survive byte-for-byte) and structs
// with no exported fields (time.Time — copying would zero them).
// Nil pointers (nullable objects such as deactivated_at) are likewise
// untouched: null is the right encoding for a missing value there.
func normalizeNilCollections(v any) any {
	rv := reflect.ValueOf(v)
	// Pointer layer: nil stays null (nullable object). A non-nil
	// pointer is normalised THROUGH the pointee and re-wrapped in a
	// fresh pointer of the same type — returning the bare pointee
	// would make a parent field Set assign, e.g., an int into an *int
	// field (manifest.Config.Brightness) and panic.
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return v
		}
		ptrType := rv.Type()
		normalized := normalizeNilCollections(rv.Elem().Interface())
		p := reflect.New(ptrType.Elem())
		p.Elem().Set(reflect.ValueOf(normalized))
		return p.Interface()
	}
	switch rv.Kind() {
	case reflect.Map:
		if rv.IsNil() {
			return reflect.MakeMap(rv.Type()).Interface()
		}
		out := reflect.MakeMap(rv.Type())
		for iter := rv.MapRange(); iter.Next(); {
			out.SetMapIndex(iter.Key(),
				reflect.ValueOf(normalizeNilCollections(iter.Value().Interface())))
		}
		return out.Interface()
	case reflect.Slice:
		if rv.IsNil() {
			// MakeSlice, NOT reflect.New(...).Elem(): the zero value of
			// a slice is nil, which would serialise back to null.
			return reflect.MakeSlice(rv.Type(), 0, 0).Interface()
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return v // byte string, not a list (see comment above)
		}
		// MakeSlice of the SAME type: a parent struct field Set would
		// panic if []manifest.Photo came back as []any.
		out := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out.Index(i).Set(reflect.ValueOf(normalizeNilCollections(rv.Index(i).Interface())))
		}
		return out.Interface()
	case reflect.Struct:
		// Opaque structs (time.Time, …) have no exported fields and
		// must pass through untouched — copying one would zero it.
		hasExported := false
		for i := 0; i < rv.NumField(); i++ {
			if rv.Type().Field(i).PkgPath == "" {
				hasExported = true
				break
			}
		}
		if !hasExported {
			return v
		}
		out := reflect.New(rv.Type()).Elem()
		for i := 0; i < rv.NumField(); i++ {
			if rv.Type().Field(i).PkgPath != "" {
				continue // unexported: not serialized either
			}
			out.Field(i).Set(reflect.ValueOf(normalizeNilCollections(rv.Field(i).Interface())))
		}
		return out.Interface()
	default:
		return v
	}
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
	case errors.Is(err, users.ErrInvalidEmail):
		return http.StatusBadRequest, "invalid_email", "email address is not valid"
	case errors.Is(err, users.ErrNameTooLong):
		return http.StatusBadRequest, "name_too_long", "display name must be at most 100 characters"
	case errors.Is(err, users.ErrNoActiveUsers):
		return http.StatusConflict, "last_active_admin", "cannot deactivate the last active administrator"
	case errors.Is(err, auth.ErrSessionExpired):
		return http.StatusUnauthorized, "session_expired", "session expired, sign in again"
	case errors.Is(err, auth.ErrSessionRevoked):
		return http.StatusUnauthorized, "session_revoked", "session was revoked, sign in again"
	case errors.Is(err, pairing.ErrWrongCode):
		return http.StatusBadRequest, "wrong_code", "the pairing code does not match the frame"
	case errors.Is(err, pairing.ErrNotPending):
		return http.StatusConflict, "challenge_consumed", "this pairing request was already handled"
	case errors.Is(err, pairing.ErrRateLimit):
		return http.StatusTooManyRequests, "rate_limited", "too many pairing requests; try again later"
	case errors.Is(err, pairing.ErrExists):
		return http.StatusConflict, "device_registered", "this frame is already registered"
	case isConfigValidationError(err):
		return http.StatusBadRequest, "invalid_config", err.Error()
	}
	return http.StatusInternalServerError, "internal", "internal server error"
}

// isConfigValidationError reports whether err is (or wraps) a
// *devices.ConfigValidationError.
func isConfigValidationError(err error) bool {
	var v *devices.ConfigValidationError
	return errors.As(err, &v)
}
