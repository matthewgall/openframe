package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/matthewgall/openframe/internal/server/albums"
)

// albumPermission: shared albums are managed by all members; private
// albums by their creator or admins (spec §5).
func (s *Server) albumPermission(w http.ResponseWriter, r *http.Request, id string) (*albums.Album, bool) {
	a, err := s.albumStore.Get(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return nil, false
	}
	u, _ := UserFrom(r.Context())
	if !a.Shared && !u.IsAdmin() && a.CreatedBy != u.ID {
		writeError(w, http.StatusForbidden, "forbidden", "album is private")
		return nil, false
	}
	return a, true
}

func (s *Server) handleListAlbums(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	all, err := s.albumStore.List(r.Context())
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	out := make([]albums.Album, 0, len(all))
	for _, a := range all {
		if a.Shared || u.IsAdmin() || a.CreatedBy == u.ID {
			out = append(out, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"albums": out})
}

type createAlbumRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Shared      *bool  `json:"shared"`
}

func (s *Server) handleCreateAlbum(w http.ResponseWriter, r *http.Request) {
	var req createAlbumRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	shared := true
	if req.Shared != nil {
		shared = *req.Shared
	}
	u, _ := UserFrom(r.Context())
	a, err := s.albumStore.Create(r.Context(), req.Name, req.Description, shared, u.ID)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "album.created", "album", a.ID, map[string]string{"name": a.Name})
	writeJSON(w, http.StatusCreated, map[string]any{"album": a})
}

func (s *Server) handleGetAlbum(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := s.albumPermission(w, r, id)
	if !ok {
		return
	}
	photoIDs, err := s.albumStore.Photos(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	devices, err := s.albumStore.Devices(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"album":   a,
		"photos":  photoIDs,
		"devices": devices,
	})
}

type updateAlbumRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Shared      *bool  `json:"shared"`
	SortOrder   int    `json:"sort_order"` // -1 = unchanged
}

func (s *Server) handleUpdateAlbum(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := s.albumPermission(w, r, id)
	if !ok {
		return
	}
	// Only the creator or an admin may edit a private album's settings;
	// shared albums are editable by everyone (spec §5).
	u, _ := UserFrom(r.Context())
	if !a.Shared && !u.IsAdmin() && a.CreatedBy != u.ID {
		writeError(w, http.StatusForbidden, "forbidden", "only the creator or an administrator may edit this album")
		return
	}
	var req updateAlbumRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	updated, err := s.albumStore.Update(r.Context(), id, req.Name, req.Description, req.Shared, req.SortOrder)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "album.updated", "album", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"album": updated})
}

func (s *Server) handleDeleteAlbum(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, ok := s.albumPermission(w, r, id)
	if !ok {
		return
	}
	u, _ := UserFrom(r.Context())
	if !u.IsAdmin() && a.CreatedBy != u.ID {
		writeError(w, http.StatusForbidden, "forbidden", "only the creator or an administrator may delete this album")
		return
	}
	if err := s.albumStore.SoftDelete(r.Context(), id); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "album.deleted", "album", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRestoreAlbum(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Deleted albums: only the creator or admin can see/restore.
	u, _ := UserFrom(r.Context())
	a, err := s.albumStore.GetIncludingDeleted(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	if a.DeletedAt == nil {
		writeError(w, http.StatusConflict, "not_deleted", "album is not deleted")
		return
	}
	if !u.IsAdmin() && a.CreatedBy != u.ID {
		s.writeDomainError(w, albums.ErrNotFound)
		return
	}
	if err := s.albumStore.Restore(r.Context(), id); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "album.restored", "album", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
}

type albumPhotoRequest struct {
	PhotoID string `json:"photo_id"`
}

// handleAlbumAddPhoto: POST /api/v1/albums/{id}/photos
// The photo must exist and not be deleted; the stored original is
// shared, never copied (spec §5).
func (s *Server) handleAlbumAddPhoto(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := s.albumPermission(w, r, id); !ok {
		return
	}
	var req albumPhotoRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.PhotoID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "photo_id is required")
		return
	}
	if _, err := s.photoStore.Get(r.Context(), req.PhotoID); err != nil {
		s.writeDomainError(w, err)
		return
	}
	if err := s.albumStore.AddPhoto(r.Context(), id, req.PhotoID); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "album.photo_added", "album", id, map[string]string{"photo": req.PhotoID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

func (s *Server) handleAlbumRemovePhoto(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := s.albumPermission(w, r, id); !ok {
		return
	}
	photoID := chi.URLParam(r, "photoId")
	if err := s.albumStore.RemovePhoto(r.Context(), id, photoID); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "album.photo_removed", "album", id, map[string]string{"photo": photoID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (s *Server) handleAlbumSetCover(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := s.albumPermission(w, r, id); !ok {
		return
	}
	var req albumPhotoRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.PhotoID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "photo_id is required")
		return
	}
	a, err := s.albumStore.SetCover(r.Context(), id, req.PhotoID)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"album": a})
}

// handleAlbumAssignDevice: POST /api/v1/albums/{id}/frames/{deviceId}
// (admin; frames become real paired devices in M5)
func (s *Server) handleAlbumAssignDevice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := s.albumPermission(w, r, id); !ok {
		return
	}
	deviceID := chi.URLParam(r, "deviceId")
	if _, err := s.deviceStore.Get(r.Context(), deviceID); err != nil {
		s.writeDomainError(w, err)
		return
	}
	if err := s.albumStore.AssignDevice(r.Context(), id, deviceID); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "album.device_assigned", "album", id, map[string]string{"device": deviceID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "assigned"})
}

func (s *Server) handleAlbumUnassignDevice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := s.albumPermission(w, r, id); !ok {
		return
	}
	deviceID := chi.URLParam(r, "deviceId")
	if err := s.albumStore.UnassignDevice(r.Context(), id, deviceID); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "album.device_unassigned", "album", id, map[string]string{"device": deviceID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "unassigned"})
}
