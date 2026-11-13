package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/matthewgall/openframe/internal/server/devices"
	"github.com/matthewgall/openframe/internal/server/manifest"
	"github.com/matthewgall/openframe/internal/server/photos"
	"github.com/matthewgall/openframe/internal/server/storage"
)

// deviceCtxKey carries the authenticated device id into handlers.
type deviceCtxKeyT struct{}

var deviceCtxKey deviceCtxKeyT

// requireDevice authenticates a frame by bearer credential
// (spec §12: frames initiate all communication; the server never
// contacts them). The device id from the path must match the
// credential; the raw token is never logged.
func (s *Server) requireDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deviceID := chi.URLParam(r, "deviceId")
		if deviceID == "" {
			writeError(w, http.StatusNotFound, "not_found", "no such device")
			return
		}
		authHeader := r.Header.Get("Authorization")
		rawToken := strings.TrimPrefix(authHeader, "Bearer ")
		if authHeader == "Bearer " {
			rawToken = ""
		}
		if err := s.deviceStore.VerifyCredential(r.Context(), deviceID, rawToken); err != nil {
			s.audit(r, "device_auth_failure", "device", deviceID, nil)
			writeError(w, http.StatusUnauthorized, "bad_device_credential", "device credential invalid or revoked")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceCtxKey, deviceID)))
	})
}

func deviceFrom(ctx context.Context) string {
	v, _ := ctx.Value(deviceCtxKey).(string)
	return v
}

// ---------------------------------------------------------------------
// GET /v1/frames/{deviceId}/manifest
//
// Conditional request protocol (spec §9): the frame stores the last
// ETag and sends If-None-Match; unchanged content returns 304 with an
// empty body so a routine 60-minute check costs one round-trip and no
// bytes.

func (s *Server) handleFrameManifest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	deviceID := deviceFrom(ctx)

	state, err := s.deviceStore.GetManifestState(ctx, deviceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such device")
		return
	}
	cfgWithVer, err := s.deviceStore.GetFrameConfig(ctx, deviceID)
	if err != nil {
		s.log.Error("manifest config load failed", "device", deviceID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "manifest unavailable")
		return
	}
	m, newState, err := manifest.Build(ctx, s.db, s.store, deviceID, state, &cfgWithVer.Config)
	if err != nil {
		if errors.Is(err, devices.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such device")
			return
		}
		s.log.Error("manifest build failed", "device", deviceID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "manifest unavailable")
		return
	}
	if err := s.deviceStore.SetManifestState(ctx, deviceID, newState); err != nil {
		s.log.Error("manifest state persist failed", "device", deviceID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "manifest unavailable")
		return
	}

	etag := manifest.ETag(m.Revision, m.ConfigRevision)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-store")
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if etagMatch(inm, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	writeJSON(w, http.StatusOK, m)
}

// handleFrameHealth: POST /api/v1/frames/{deviceId}/health (device auth).
// The frame's periodic self-report (spec §13) doubles as its heartbeat:
// the response carries the remote-sync request sequence, which is how
// an administrator's "trigger synchronisation" reaches a frame without
// any inbound connection (spec §6, §12).
func (s *Server) handleFrameHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	deviceID := deviceFrom(ctx)

	var h devices.HealthReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&h); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid health payload")
		return
	}
	h.Sanitize()
	if err := s.deviceStore.ReportHealth(ctx, deviceID, &h); err != nil {
		s.log.Error("health report failed", "device", deviceID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record health")
		return
	}
	seq, err := s.deviceStore.GetSyncRequestSeq(ctx, deviceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such device")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"sync_request_seq": seq,
		"server_time":      time.Now().UTC().Format(time.RFC3339),
	})
}

// etagMatch implements the subset of RFC 9110 If-None-Match semantics
// we need: exact tag, weak-tag-insensitive comparison, list form, and
// the "*" wildcard.
func etagMatch(ifNoneMatch, etag string) bool {
	for _, cand := range strings.Split(ifNoneMatch, ",") {
		c := strings.TrimSpace(cand)
		if c == "" {
			continue
		}
		if c == "*" {
			return true
		}
		c = strings.TrimPrefix(c, "W/")
		e := strings.TrimPrefix(etag, "W/")
		if c == e {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// GET /v1/frames/{deviceId}/photos/{hash}.{variant}
//
// Authenticated derivative download (spec §9 "Download safety" server
// side; "authenticated resource identifiers" in the manifest protocol).
// Authorization is strict: the object must exist AND be referenced by
// one of the device's assigned albums — frames cannot use their
// credential to browse the library. Single-range requests are
// supported for resumable downloads.

func (s *Server) handleFramePhoto(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	deviceID := deviceFrom(ctx)

	// Resource is "<original-hash>.<variant>" (manifest File.Resource).
	resource := chi.URLParam(r, "resource")
	dot := strings.LastIndex(resource, ".")
	if dot <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "unknown resource")
		return
	}
	hash, variant := resource[:dot], resource[dot+1:]
	if !isFrameVariant(variant) {
		writeError(w, http.StatusNotFound, "not_found", "unknown resource")
		return
	}
	key := storage.Key(hash + "." + variant)

	if !s.framePhotoAssigned(ctx, deviceID, hash) {
		s.audit(r, "frame_download_denied", "device", deviceID, map[string]any{"photo": hash})
		writeError(w, http.StatusNotFound, "not_found", "unknown resource")
		return
	}

	total, err := s.store.Size(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "unknown resource")
			return
		}
		s.log.Error("frame download stat failed", "device", deviceID, "key", key, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "download unavailable")
		return
	}

	offset := int64(0)
	status := http.StatusOK
	var end int64 = total - 1
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		rs, re, ok := parseByteRange(rangeHeader, total)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable", "range not satisfiable")
			return
		}
		offset, end = rs, min(re, rs+maxRangeBytes-1)
		status = http.StatusPartialContent
	}

	rc, err := s.store.OpenAt(ctx, key, offset)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "unknown resource")
			return
		}
		s.log.Error("frame download open failed", "device", deviceID, "key", key, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "download unavailable")
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end, total))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(end-offset+1, 10))
	w.WriteHeader(status)

	buf := make([]byte, 32<<10)
	written := int64(0)
	lim := end - offset + 1
	for written < lim {
		n := lim - written
		if int64(len(buf)) < n {
			n = int64(len(buf))
		}
		got, err := io.ReadFull(rc, buf[:n])
		written += int64(got)
		if _, werr := w.Write(buf[:got]); werr != nil {
			return // client went away
		}
		if err != nil {
			if err != io.ErrUnexpectedEOF && err != io.EOF {
				s.log.Warn("frame download short read", "device", deviceID, "key", key, "error", err)
			}
			return
		}
	}
	s.audit(r, "frame_download", "device", deviceID, map[string]any{"photo": hash, "variant": variant})
}

// maxRangeBytes bounds a single range read (resumed chunks); a client
// that needs more re-requests. Keeps handler memory and a single
// slow client bounded.
const maxRangeBytes = int64(4 << 20)

func isFrameVariant(v string) bool {
	return v == storage.VariantDisplay || v == storage.VariantThumb
}

// framePhotoAssigned reports whether the photo (by original hash) is a
// live member of at least one album assigned to the device.
func (s *Server) framePhotoAssigned(ctx context.Context, deviceID, hash string) bool {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM photos p
		   JOIN album_photos ap ON ap.photo_id = p.id
		   JOIN album_devices ad ON ad.album_id = ap.album_id
		  WHERE p.hash = ? AND p.deleted_at IS NULL
		    AND ad.device_id = ?
		    AND EXISTS (SELECT 1 FROM albums a WHERE a.id = ap.album_id AND a.deleted_at IS NULL)`,
		hash, deviceID).Scan(&n)
	if err != nil {
		return false
	}
	return n > 0
}

// parseByteRange parses a single "bytes=start-end" range against a
// total-size object. Multi-range and open-ended suffix ranges beyond
// the last maxRangeBytes are not supported (frames request
// start-qualified ranges).
func parseByteRange(header string, total int64) (start, end int64, ok bool) {
	header = strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	parts := strings.Split(header, "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "" && parts[1] != "" {
		// Suffix range "bytes=-N": last N bytes.
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 || n > total {
			return 0, 0, false
		}
		return total - n, total - 1, true
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= total {
		return 0, 0, false
	}
	if parts[1] == "" {
		return start, total - 1, true
	}
	end, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	if end >= total {
		end = total - 1
	}
	return start, end, true
}

// ---------------------------------------------------------------------
// POST /v1/frames/{deviceId}/changes
//
// Idempotent ingest of local frame changes (spec §9 "Local changes").
// M3 carries favourites (spec §5: per-frame state). Conflict rule is
// explicit last-write-wins on the frame's change timestamp; the op id
// makes replays after partial failures safe.

type changeOp struct {
	OpID      string `json:"op_id"`
	Type      string `json:"type"`
	PhotoHash string `json:"photo_hash"`
	Value     *bool  `json:"value,omitempty"`
	TS        string `json:"ts"` // RFC3339 when the change happened on the frame
}

type changeRequest struct {
	Ops []changeOp `json:"ops"`
}

type changeResponse struct {
	Accepted    int      `json:"accepted"`
	Duplicate   int      `json:"duplicates"`
	Rejected    int      `json:"rejected"`
	RejectedOps []string `json:"rejected_ops,omitempty"`
}

func (s *Server) handleFrameChanges(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	deviceID := deviceFrom(ctx)

	var req changeRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed change request")
		return
	}
	if len(req.Ops) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "no operations")
		return
	}
	if len(req.Ops) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_request", "too many operations (max 500)")
		return
	}

	resp := changeResponse{}
	for _, op := range req.Ops {
		reject := func() {
			resp.Rejected++
			if op.OpID != "" {
				resp.RejectedOps = append(resp.RejectedOps, op.OpID)
			}
		}
		if op.OpID == "" || len(op.OpID) > 64 || op.PhotoHash == "" {
			reject()
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, op.TS)
		if err != nil || ts.IsZero() {
			reject()
			continue
		}
		claimed, err := s.deviceStore.ClaimOp(ctx, deviceID, op.OpID)
		if err != nil {
			s.log.Error("claim op failed", "device", deviceID, "op", op.OpID, "error", err)
			writeError(w, http.StatusInternalServerError, "internal", "change ingest failed")
			return
		}
		if !claimed {
			resp.Duplicate++
			continue
		}
		switch op.Type {
		case "favourite":
			if op.Value == nil {
				reject()
				continue
			}
			if err := s.deviceStore.SetFavourite(ctx, deviceID, op.PhotoHash, *op.Value, ts); err != nil {
				s.log.Error("set favourite failed", "device", deviceID, "photo", op.PhotoHash, "error", err)
				reject()
				continue
			}
		default:
			reject()
			continue
		}
		resp.Accepted++
	}
	s.audit(r, "frame_changes", "device", deviceID, map[string]any{
		"accepted": resp.Accepted, "duplicates": resp.Duplicate, "rejected": resp.Rejected,
	})
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------
// Admin: device credential lifecycle (M3). Full pairing UX (challenge
// codes + approval) arrives in M5; these endpoints are what the
// pairing flow will build on (spec §12 steps 4-7).

type credentialResponse struct {
	Token    string `json:"token"`
	IssuedAt string `json:"issued_at"`
}

func (s *Server) handleIssueDeviceCredential(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "id")
	token, err := s.deviceStore.IssueCredential(r.Context(), deviceID)
	if err != nil {
		if errors.Is(err, devices.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such device")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_device", err.Error())
		return
	}
	s.audit(r, "device_credential_issued", "device", deviceID, nil)
	writeJSON(w, http.StatusCreated, credentialResponse{
		Token:    token,
		IssuedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleRevokeDeviceCredential(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "id")
	if err := s.deviceStore.RevokeCredential(r.Context(), deviceID); err != nil {
		if errors.Is(err, devices.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such device or credential")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "revocation failed")
		return
	}
	s.audit(r, "device_credential_revoked", "device", deviceID, nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleSetPhotoPriority / handleSetAlbumPriority expose spec §9
// priority delivery to admins.

type priorityRequest struct {
	Priority bool `json:"priority"`
}

func (s *Server) handleSetPhotoPriority(w http.ResponseWriter, r *http.Request) {
	var req priorityRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "expected {\"priority\": bool}")
		return
	}
	if err := s.photoStore.SetPriority(r.Context(), chi.URLParam(r, "id"), req.Priority); err != nil {
		if errors.Is(err, photos.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such photo")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "update failed")
		return
	}
	s.audit(r, "photo_priority", "photo", chi.URLParam(r, "id"), map[string]any{"priority": req.Priority})
	writeJSON(w, http.StatusOK, map[string]bool{"priority": req.Priority})
}

func (s *Server) handleSetAlbumPriority(w http.ResponseWriter, r *http.Request) {
	var req priorityRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "expected {\"priority\": bool}")
		return
	}
	if err := s.albumStore.SetPriority(r.Context(), chi.URLParam(r, "id"), req.Priority); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such album")
		return
	}
	s.audit(r, "album_priority", "album", chi.URLParam(r, "id"), map[string]any{"priority": req.Priority})
	writeJSON(w, http.StatusOK, map[string]bool{"priority": req.Priority})
}
