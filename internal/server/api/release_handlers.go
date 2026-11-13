package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/matthewgall/openframe/internal/server/releases"
	"github.com/matthewgall/openframe/internal/updater"
)

// Release API (M6, spec §14).
//
// Public (frame-facing): frames fetch signed metadata and artifacts.
// These endpoints are auth-free on purpose — releases are
// self-authenticating (Ed25519 signature against device-pinned keys),
// contain no personal data, and an unavailable server must never gate
// slideshow/sync. The admin group below is session+CSRF protected.

const maxUploadBytes = releases.MaxArtifactBytes + 1<<20 // metadata + slack

// latestResponse is the latest.json body: the signed release document
// verbatim plus an explicitly-unsigned serving block.
//
// The serving block carries the operator-controlled staged-rollout
// values. It is outside the signature on purpose: rollout expansion
// (10% → 25% → 100%) is a policy decision that happens after the
// release is signed, and it can only change WHO receives the update —
// never WHAT (version, artifact digest and size live in the signed
// document, and the frame's trust checks are authoritative).
type latestResponse struct {
	Release json.RawMessage `json:"release"`
	Serving struct {
		RolloutPercent int      `json:"rollout_percent"`
		RolloutDevices []string `json:"rollout_devices,omitempty"`
	} `json:"serving"`
}

// handleLatestRelease serves GET /v1/releases/{channel}/latest.json.
// The signed metadata is served VERBATIM (the server never
// re-serialises) so the frame's signature check covers exactly the
// document that was signed.
func (s *Server) handleLatestRelease(w http.ResponseWriter, r *http.Request) {
	channel := chi.URLParam(r, "channel")
	osName, arch := r.URL.Query().Get("os"), r.URL.Query().Get("arch")
	if channel == "" || osName == "" || arch == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "channel, os and arch are required")
		return
	}
	if !updater.ValidChannel(channel) {
		writeError(w, http.StatusNotFound, "not_found", "unknown channel")
		return
	}
	rel, err := s.releaseStore.Latest(r.Context(), channel, osName, arch, time.Now().UTC())
	if errors.Is(err, releases.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no release published for this channel and platform")
		return
	}
	if err != nil {
		s.log.Error("latest release", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "failed to load release")
		return
	}
	var out latestResponse
	out.Release = rel.Metadata
	out.Serving.RolloutPercent = rel.RolloutPercent
	out.Serving.RolloutDevices = rel.RolloutDevices
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// handleReleaseArtifact serves GET /v1/releases/{version}/artifact
// with Range support (http.ServeContent) so interrupted downloads can
// resume on flaky links.
func (s *Server) handleReleaseArtifact(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")
	osName, arch := r.URL.Query().Get("os"), r.URL.Query().Get("arch")
	if osName == "" || arch == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "os and arch are required")
		return
	}
	f, _, err := s.releaseStore.Artifact(version, osName, arch)
	if errors.Is(err, releases.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "artifact not found")
		return
	}
	if err != nil {
		s.log.Error("release artifact", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "failed to open artifact")
		return
	}
	defer f.Close()
	// The artifact was validated against the signed digest at upload;
	// the frame re-validates after download (defence in depth).
	http.ServeContent(w, r, "openframe-"+osName+"-"+arch, time.Time{}, f)
}

// --- admin ---

// handleListReleases: GET /v1/releases
func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	list, err := s.releaseStore.List(r.Context())
	if err != nil {
		s.log.Error("list releases", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "failed to list releases")
		return
	}
	if list == nil {
		list = []releases.Release{}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleUploadRelease: POST /v1/releases (multipart: "metadata" JSON
// field + "artifact" file). The upload lands in staged state; frames
// never see it until an admin approves it.
func (s *Server) handleUploadRelease(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upload", "expected multipart form with 'metadata' and 'artifact'")
		return
	}
	metaField := r.FormValue("metadata")
	if metaField == "" {
		writeError(w, http.StatusBadRequest, "invalid_upload", "missing 'metadata' field")
		return
	}
	file, header, err := r.FormFile("artifact")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upload", "missing 'artifact' file")
		return
	}
	defer file.Close()
	if header.Size > releases.MaxArtifactBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "artifact_too_large", "artifact exceeds the limit")
		return
	}
	artifact, err := io.ReadAll(io.LimitReader(file, releases.MaxArtifactBytes+1))
	if err != nil {
		s.log.Error("release upload read", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "failed to read upload")
		return
	}
	if int64(len(artifact)) > releases.MaxArtifactBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "artifact_too_large", "artifact exceeds the limit")
		return
	}
	rel, err := s.releaseStore.Upload(r.Context(), []byte(metaField), artifact)
	if errors.Is(err, releases.ErrDuplicate) {
		writeError(w, http.StatusConflict, "duplicate_release", "this version/channel/platform is already uploaded")
		return
	}
	if err != nil {
		s.log.Warn("release upload rejected", "error", err)
		writeError(w, http.StatusBadRequest, "invalid_release", err.Error())
		return
	}
	s.audit(r, "release.upload", "release", rel.Version,
		map[string]string{"channel": rel.Channel, "status": rel.Status})
	writeJSON(w, http.StatusCreated, rel)
}

// handleApproveRelease: POST /v1/releases/{version}/approve
func (s *Server) handleApproveRelease(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	version := chi.URLParam(r, "version")
	rel, err := s.releaseStore.Approve(r.Context(), version, q.Get("channel"), q.Get("os"), q.Get("arch"))
	s.finishReleaseMutation(w, r, rel, err, "release.approve")
}

// handlePauseRelease: POST /v1/releases/{version}/pause — the staged
// rollout kill switch.
func (s *Server) handlePauseRelease(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	version := chi.URLParam(r, "version")
	rel, err := s.releaseStore.Pause(r.Context(), version, q.Get("channel"), q.Get("os"), q.Get("arch"))
	s.finishReleaseMutation(w, r, rel, err, "release.pause")
}

// handleSetReleaseRollout: PUT /v1/releases/{version}/rollout
// body: {"percent": 25, "devices": ["id1"]}.
func (s *Server) handleSetReleaseRollout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Percent int      `json:"percent"`
		Devices []string `json:"devices"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "expected {percent, devices}")
		return
	}
	q := r.URL.Query()
	version := chi.URLParam(r, "version")
	rel, err := s.releaseStore.SetRollout(r.Context(), version, q.Get("channel"), q.Get("os"), q.Get("arch"), body.Percent, body.Devices)
	s.finishReleaseMutation(w, r, rel, err, "release.rollout")
}

// handleDeleteRelease: DELETE /v1/releases/{version}
func (s *Server) handleDeleteRelease(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	version := chi.URLParam(r, "version")
	err := s.releaseStore.Delete(r.Context(), version, q.Get("channel"), q.Get("os"), q.Get("arch"))
	if errors.Is(err, releases.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "release not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, "delete_refused", err.Error())
		return
	}
	s.audit(r, "release.delete", "release", version, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) finishReleaseMutation(w http.ResponseWriter, r *http.Request, rel *releases.Release, err error, action string) {
	if errors.Is(err, releases.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "release not found")
		return
	}
	if err != nil {
		s.log.Error(action, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "release operation failed")
		return
	}
	s.audit(r, action, "release", rel.Version,
		map[string]string{"channel": rel.Channel, "status": rel.Status, "latest": boolStr(rel.IsLatest)})
	writeJSON(w, http.StatusOK, rel)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
