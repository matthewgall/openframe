package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/matthewgall/openframe/internal/server/photos"
	"github.com/matthewgall/openframe/internal/server/storage"
)

// handleUpload: POST /api/v1/photos/upload
//
// Multipart form, field "files", one or more parts. Each file is:
// size-capped, magic-byte validated, decoded (dimension-capped),
// orientation-applied, hashed, stored content-addressed, and
// derivative-generated. Duplicates (same content hash) link to the
// existing original instead of storing it again.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())

	maxFiles := s.cfg.Server.MaxUploadFiles
	maxBytes := s.cfg.Server.MaxUploadBytes
	// Bound the whole request body (parts + multipart overhead).
	limit := maxBytes*int64(maxFiles) + 1<<20
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if isBodyTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "upload_too_large",
				"upload exceeds the size limit")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart upload: "+err.Error())
		return
	}

	fileMap := r.MultipartForm.File["files"]
	files := fileMap
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "no files provided (field name \"files\")")
		return
	}
	if len(files) > maxFiles {
		writeError(w, http.StatusRequestEntityTooLarge, "too_many_files",
			fmt.Sprintf("at most %d files per upload", maxFiles))
		return
	}

	created, failed := make([]map[string]any, 0, len(files)), make([]map[string]any, 0)
	for _, fh := range files {
		name := sanitizeFilename(fh.Filename)
		data, err := readUploadPart(fh, maxBytes)
		if err != nil {
			failed = append(failed, map[string]any{"name": name, "error": err.Error()})
			continue
		}

		md, original, derivs, err := photos.Process(r.Context(), data, s.pipeOpts)
		if err != nil {
			status, code, msg := domainError(err)
			failed = append(failed, map[string]any{"name": name, "error": msg, "code": code})
			_ = status
			continue
		}

		if err := s.putOriginal(r, storage.Key(md.Hash), original); err != nil {
			s.log.Error("store original failed", "hash", md.Hash, "error", err)
			failed = append(failed, map[string]any{"name": name, "error": "storage write failed", "code": "internal"})
			continue
		}
		for variant, b := range derivs {
			key := storage.Key(md.Hash + "." + variant)
			if exists, _ := s.store.Exists(r.Context(), key); !exists {
				if err := s.store.Put(r.Context(), key, strings.NewReader(string(b))); err != nil {
					s.log.Error("store derivative failed", "key", key, "error", err)
				}
			}
		}

		p, dup, err := s.photoStore.Create(r.Context(), md, u.ID, name)
		if err != nil {
			failed = append(failed, map[string]any{"name": name, "error": "database error", "code": "internal"})
			continue
		}
		entry := map[string]any{"photo": p, "duplicate": dup}
		created = append(created, entry)
	}

	s.audit(r, "photos.uploaded", "photos", "", map[string]int{"ok": len(created), "failed": len(failed)})

	status := http.StatusCreated
	if len(created) == 0 {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]any{"created": created, "failed": failed})
}

// putOriginal stores the original bytes, verifying the content hash
// against the key (storage enforces this for bare-hash keys). Existing
// objects are skipped (shared originals).
func (s *Server) putOriginal(r *http.Request, key storage.Key, data []byte) error {
	exists, err := s.store.Exists(r.Context(), key)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return s.store.Put(r.Context(), key, bytes.NewReader(data))
}

func readUploadPart(fh *multipart.FileHeader, maxBytes int64) ([]byte, error) {
	if fh.Size > maxBytes {
		return nil, photos.ErrTooLarge
	}
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, photos.ErrTooLarge
	}
	return data, nil
}

// sanitizeFilename keeps only a safe base name (no paths, no odd
// bytes). The name is display metadata only, never a storage key.
func sanitizeFilename(name string) string {
	base := filepath.Base(name)
	base = strings.ReplaceAll(base, "..", "")
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "photo"
	}
	if len(base) > 255 {
		base = base[len(base)-255:]
	}
	return base
}

// handleListPhotos: GET /api/v1/photos?search=&album=&deleted=1&limit=&offset=
func (s *Server) handleListPhotos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	params := photos.ListParams{
		Search:  q.Get("search"),
		AlbumID: q.Get("album"),
		Deleted: q.Get("deleted") == "1",
		Limit:   limit,
		Offset:  offset,
	}
	// The recovery view is permission-scoped below.
	ph, total, err := s.photoStore.List(r.Context(), params)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	u, _ := UserFrom(r.Context())
	if params.Deleted && !u.IsAdmin() {
		// Non-admins only recover their own uploads.
		kept := ph[:0]
		for _, p := range ph {
			if p.UploadedBy == u.ID {
				kept = append(kept, p)
			}
		}
		ph = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"photos": ph, "total": total})
}

func (s *Server) handleGetPhoto(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, err := s.photoStore.Get(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"photo": p})
}

type updatePhotoRequest struct {
	Caption     string `json:"caption"`
	CaptureDate string `json:"capture_date"` // RFC3339
}

// handleUpdatePhoto: PATCH /api/v1/photos/{id} (caption / capture date)
func (s *Server) handleUpdatePhoto(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req updatePhotoRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	var capture *time.Time
	if req.CaptureDate != "" {
		t, err := time.Parse(time.RFC3339, req.CaptureDate)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "capture_date must be RFC3339")
			return
		}
		capture = &t
	}
	p, err := s.photoStore.UpdateMetadata(r.Context(), id, req.Caption, capture)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "photo.updated", "photo", id, map[string]bool{"caption": req.Caption != "", "capture_date": req.CaptureDate != ""})
	writeJSON(w, http.StatusOK, map[string]any{"photo": p})
}

// canTouchPhoto loads a photo (including deleted) and enforces the
// visibility rule: live photos are visible to all members; deleted
// photos only to their uploader or an admin.
func (s *Server) canTouchPhoto(w http.ResponseWriter, r *http.Request, id string) *photos.Photo {
	u, _ := UserFrom(r.Context())
	p, err := s.photoStore.GetIncludingDeleted(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return nil
	}
	if p.DeletedAt != nil && !u.IsAdmin() && p.UploadedBy != u.ID {
		s.writeDomainError(w, photos.ErrNotFound)
		return nil
	}
	return p
}

// handleDeletePhoto: DELETE /api/v1/photos/{id} (soft delete).
// Members may delete their own uploads; admins any.
func (s *Server) handleDeletePhoto(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	u, _ := UserFrom(r.Context())
	p := s.canTouchPhoto(w, r, id)
	if p == nil {
		return
	}
	if !u.IsAdmin() && p.UploadedBy != u.ID {
		writeError(w, http.StatusForbidden, "forbidden", "you can only delete your own photographs")
		return
	}
	if err := s.photoStore.SoftDelete(r.Context(), id); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "photo.deleted", "photo", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleRestorePhoto: POST /api/v1/photos/{id}/restore
func (s *Server) handleRestorePhoto(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	u, _ := UserFrom(r.Context())
	p := s.canTouchPhoto(w, r, id)
	if p == nil {
		return
	}
	if p.DeletedAt == nil {
		writeError(w, http.StatusConflict, "not_deleted", "photograph is not deleted")
		return
	}
	if !u.IsAdmin() && p.UploadedBy != u.ID {
		writeError(w, http.StatusForbidden, "forbidden", "you can only restore your own photographs")
		return
	}
	if err := s.photoStore.Restore(r.Context(), id); err != nil {
		s.writeDomainError(w, err)
		return
	}
	s.audit(r, "photo.restored", "photo", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
}

// handlePhotoFile: GET /api/v1/photos/{id}/file?variant=...
//
// Variants: original | display | thumb | portrait | landscape.
//   - original requires the uploader or an admin (unstripped EXIF)
//   - derivatives are available to all active members
//   - deleted photos only to uploader/admin
func (s *Server) handlePhotoFile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	variant := r.URL.Query().Get("variant")
	if variant == "" {
		variant = storage.VariantDisplay
	}

	u, _ := UserFrom(r.Context())
	p, err := s.photoStore.GetIncludingDeleted(r.Context(), id)
	if err != nil {
		s.writeDomainError(w, err)
		return
	}
	if p.DeletedAt != nil && !u.IsAdmin() && p.UploadedBy != u.ID {
		s.writeDomainError(w, photos.ErrNotFound)
		return
	}

	var key storage.Key
	mime := "application/octet-stream"
	switch variant {
	case "original":
		if !u.IsAdmin() && p.UploadedBy != u.ID {
			writeError(w, http.StatusForbidden, "forbidden", "original download is limited to the uploader or administrators")
			return
		}
		key = storage.Key(p.Hash)
		mime = p.MIME
	case storage.VariantDisplay, storage.VariantThumb, storage.VariantPortrait, storage.VariantLandscape:
		key = storage.Key(p.Hash + "." + variant)
		mime = "image/jpeg"
	default:
		writeError(w, http.StatusBadRequest, "bad_request",
			"variant must be original|display|thumb|portrait|landscape")
		return
	}

	rc, err := s.store.Open(r.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Derivatives are regenerable; the original is not.
			// Report clearly so clients can show a loading error state.
			writeError(w, http.StatusNotFound, "file_missing", "file is not in storage")
			return
		}
		s.log.Error("open stored file failed", "key", key, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read file")
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil && r.Context().Err() == nil {
		s.log.Warn("file stream interrupted", "key", key, "error", err)
	}
}
