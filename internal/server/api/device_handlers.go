package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/matthewgall/openframe/internal/server/audit"
	"github.com/matthewgall/openframe/internal/server/devices"
)

// Device endpoints (admin): registry, health, per-frame configuration
// (spec §6, §13).

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.deviceStore.ListWithHealth(r.Context())
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	if devs == nil {
		devs = []*devices.HealthWithDevice{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devs})
}

// handleDeviceDetail: GET /api/v1/devices/{id} (admin) — the full
// frame view: registry row, latest health report, derived status, and
// the stored per-frame configuration.
func (s *Server) handleDeviceDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	d, err := s.deviceStore.Get(ctx, id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	health, err := s.deviceStore.GetHealth(ctx, id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	hb, err := s.deviceStore.LastHeartbeat(ctx, id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	seq, err := s.deviceStore.GetSyncRequestSeq(ctx, id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	cfg, err := s.deviceStore.GetFrameConfig(ctx, id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device":            d,
		"health":            health,
		"last_heartbeat_at": hb,
		"status":            devices.DeriveStatus(hb, health, time.Now().UTC()),
		"sync_request_seq":  seq,
		"config":            cfg,
	})
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

// handleDeviceConfig: GET/PUT /api/v1/devices/{id}/config (admin).
// PUT validates the document (devices.FrameConfig.Validate) and bumps
// the config version; frames pick it up on their next manifest fetch.
func (s *Server) handleDeviceConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	if r.Method == http.MethodPut {
		var cfg devices.FrameConfig
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&cfg); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		set, err := s.deviceStore.SetFrameConfig(ctx, id, cfg)
		if err != nil {
			s.writeDomainError(w, err)
			return
		}
		s.audit(r, "device.config_updated", "device", id, map[string]string{"version": strconv.FormatInt(set.Version, 10)})
		writeJSON(w, http.StatusOK, map[string]any{"config": set})
		return
	}

	cfg, err := s.deviceStore.GetFrameConfig(ctx, id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": cfg})
}

// handleDeviceSync: POST /api/v1/devices/{id}/sync (admin) — remote
// sync request (spec §6). Bumps the device's sync sequence; the frame
// acts on it at its next heartbeat (within ~30s).
func (s *Server) handleDeviceSync(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	seq, err := s.deviceStore.RequestSync(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "device.sync_requested", "device", id, map[string]string{"seq": strconv.FormatInt(seq, 10)})
	writeJSON(w, http.StatusOK, map[string]any{"status": "requested", "sync_request_seq": seq})
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
