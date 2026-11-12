package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/matthewgall/openframe/internal/server/audit"
	"github.com/matthewgall/openframe/internal/server/devices"
)

// M2 device endpoints (admin): minimal registry for album assignment.
// Pairing, credentials and health arrive in M5.

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.deviceStore.List(r.Context())
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	if devs == nil {
		devs = []devices.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devs})
}

type createDeviceRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateDevice(w http.ResponseWriter, r *http.Request) {
	var req createDeviceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	d, err := s.deviceStore.Create(r.Context(), req.Name)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "device.created", "device", d.ID, map[string]string{"name": d.Name})
	writeJSON(w, http.StatusCreated, map[string]any{"device": d})
}

type updateDeviceRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleUpdateDevice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req updateDeviceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	if err := s.deviceStore.Rename(r.Context(), id, req.Name); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "device.renamed", "device", id, nil)
	d, err := s.deviceStore.Get(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": d})
}

func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.deviceStore.Revoke(r.Context(), id); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "device.revoked", "device", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleAudit: GET /api/v1/audit?limit=&offset= (admin).
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 100
	}
	entries, err := s.auditRec.List(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read audit log")
		return
	}
	if entries == nil {
		entries = []audit.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "offset_applied": offset})
}
