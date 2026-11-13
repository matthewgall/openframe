package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/matthewgall/openframe/internal/server/pairing"
)

// Pairing protocol endpoints (spec §12).
//
// Public (frame-facing, unauthenticated): a not-yet-paired frame has no
// credential by definition. Creation is rate-limited per source
// address (store-level) to keep a rogue frame from flooding the admin
// UI or guessing.
//
// Admin: approval requires a valid admin session AND the code shown on
// the physical frame — an attacker who can reach the API cannot approve
// a device they cannot see.

// POST /api/v1/devices/pair-challenges  (frame, public)
// Body: {"device_id": "<frame identity uuid>", "device_name": "..."}
// 201: {"challenge_id", "code", "expires_at", "ttl_seconds"}
func (s *Server) handlePairChallenge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID   string `json:"device_id"`
		DeviceName string `json:"device_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	created, err := s.pairingStore.Create(r.Context(), req.DeviceID, req.DeviceName, clientIP(r))
	if err != nil {
		if errors.Is(err, pairing.ErrExists) {
			// Restart recovery: the frame was registered earlier. If a
			// challenge is still resolvable (e.g. approved but the one-
			// time credential never claimed), hand its id back so the
			// frame can resume polling and claim it.
			if ch, lerr := s.pairingStore.LatestForDevice(r.Context(), req.DeviceID); lerr == nil && ch != nil {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error":        "device_registered",
					"challenge_id": ch.ID,
				})
				return
			}
		}
		s.writeDomainError(w, err)
		return
	}
	s.log.Info("pairing challenge created", "device", created.DeviceID, "ip", created.SourceIP)
	writeJSON(w, http.StatusCreated, map[string]any{
		"challenge_id": created.ID,
		"code":         created.Code,
		"expires_at":   created.ExpiresAt.Format("2006-01-02T15:04:05Z"),
		"ttl_seconds":  int(pairing.ChallengeTTL.Seconds()),
	})
}

// POST /api/v1/devices/pair-status  (frame, public)
// Body: {"challenge_id": "..."}
// 200: {"status": "pending|approved|rejected|expired", "device_id" (when approved)}
func (s *Server) handlePairStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChallengeID string `json:"challenge_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	status, deviceID, credential, err := s.pairingStore.Status(r.Context(), req.ChallengeID)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	resp := map[string]any{"status": status}
	if status == pairing.StatusApproved && deviceID != "" {
		resp["device_id"] = deviceID
		// One-time claim: the credential appears exactly once, on the
		// first poll after approval.
		if credential != "" {
			resp["credential"] = credential
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /api/v1/pairing/pending  (admin)
func (s *Server) handleListPairing(w http.ResponseWriter, r *http.Request) {
	pending, err := s.pairingStore.ListPending(r.Context())
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	if pending == nil {
		pending = []pairing.Challenge{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"challenges": pending})
}

// POST /api/v1/pairing/{id}/approve  (admin)
// Body: {"code": "XXXXXXXX"}
// 201: {"device": {...}, "credential": "ofdv1_..." (shown once)}
func (s *Server) handleApprovePairing(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	deviceID, token, err := s.pairingStore.Approve(r.Context(), id, req.Code)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "pairing.approved", "device", deviceID, map[string]string{"challenge": id})

	d, err := s.deviceStore.Get(r.Context(), deviceID)
	if err != nil {
		// The device was just created inside Approve; this should not
		// happen. The credential is returned anyway (one-time display).
		writeJSON(w, http.StatusCreated, map[string]any{"credential": token})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"device": d, "credential": token})
}

// POST /api/v1/pairing/{id}/reject  (admin)
func (s *Server) handleRejectPairing(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.pairingStore.Reject(r.Context(), id); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "pairing.rejected", "pairing", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}
