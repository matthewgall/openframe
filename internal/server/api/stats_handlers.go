package api

import (
	"net/http"
	"os"
	"time"

	"github.com/matthewgall/openframe/internal/version"
)

// handleStats: GET /api/v1/stats — the dashboard metrics (spec §16).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var photosTotal, photosDeleted, albumsTotal, albumsShared, usersTotal, devicesTotal int
	scanCount := func(table, where string) int {
		q := `SELECT COUNT(*) FROM ` + table
		if where != "" {
			q += " WHERE " + where
		}
		var n int
		if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return 0
		}
		return n
	}
	photosTotal = scanCount("photos", "")
	photosDeleted = scanCount("photos", "deleted_at IS NOT NULL")
	albumsTotal = scanCount("albums", "deleted_at IS NULL")
	albumsShared = scanCount("albums", "deleted_at IS NULL AND is_shared = 1")
	usersTotal = scanCount("users", "deactivated_at IS NULL")
	devicesTotal = scanCount("devices", "")

	var storageBytes int64
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM photos WHERE deleted_at IS NULL`).Scan(&storageBytes)

	// Last 10 uploads (most recent first).
	type recentPhoto struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		ThumbURL    string `json:"thumb_url"`
		UploadedBy  string `json:"uploaded_by"`
		CreatorName string `json:"uploaded_by_name"`
		UploadedAt  string `json:"uploaded_at"`
	}
	recent := make([]recentPhoto, 0, 10)
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id, p.original_name, p.uploaded_by, u.display_name, p.created_at
		   FROM photos p JOIN users u ON u.id = p.uploaded_by
		  WHERE p.deleted_at IS NULL
		  ORDER BY p.created_at DESC LIMIT 10`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var rp recentPhoto
			var name, uploadedAt string
			if err := rows.Scan(&rp.ID, &name, &rp.UploadedBy, &rp.CreatorName, &uploadedAt); err == nil {
				rp.Name = name
				rp.ThumbURL = "/api/v1/photos/" + rp.ID + "/file?variant=thumb"
				t, perr := time.Parse(time.RFC3339, uploadedAt)
				if perr == nil {
					rp.UploadedAt = t.UTC().Format(time.RFC3339)
				}
			}
			recent = append(recent, rp)
		}
	}

	hostname, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{
		"photos": map[string]any{
			"total":         photosTotal,
			"deleted":       photosDeleted,
			"storage_bytes": storageBytes,
			"recent":        recent,
		},
		"albums":  map[string]any{"total": albumsTotal, "shared": albumsShared},
		"users":   map[string]any{"active": usersTotal},
		"devices": map[string]any{"total": devicesTotal},
		"server": map[string]any{
			"version":  version.Version,
			"hostname": hostname,
		},
	})
}
