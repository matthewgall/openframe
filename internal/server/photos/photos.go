package photos

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/id"
)

// ErrNotFound is returned when a photo does not exist (or is hidden
// for the caller — the API layer treats both as 404 for deleted items
// of other users per permission rules).
var ErrNotFound = errors.New("photos: not found")

// Photo is the domain record.
type Photo struct {
	ID           string     `json:"id"`
	Hash         string     `json:"hash"`
	OriginalName string     `json:"original_name"`
	MIME         string     `json:"mime"`
	Width        int        `json:"width"`
	Height       int        `json:"height"`
	SizeBytes    int64      `json:"size_bytes"`
	CaptureDate  *time.Time `json:"capture_date,omitempty"`
	Caption      string     `json:"caption"`
	UploadedBy   string     `json:"uploaded_by"`
	UploaderName string     `json:"uploader_name,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
	// Albums this photo belongs to (populated by List/Get with albums).
	Albums []string `json:"albums,omitempty"`
}

// Store persists photos.
type Store struct{ dbh *sql.DB }

func NewStore(dbh *sql.DB) *Store { return &Store{dbh: dbh} }

// Create inserts a photo record. If the hash already exists (duplicate
// upload), it returns the existing photo and dup=true — the original
// object is shared, never copied (spec §5).
func (s *Store) Create(ctx context.Context, md Metadata, uploaderID, originalName string) (photo *Photo, dup bool, err error) {
	pid, err := id.NewUUID()
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	capture := ""
	if md.CaptureDate != nil {
		capture = md.CaptureDate.UTC().Format(time.RFC3339)
	}
	_, err = s.dbh.ExecContext(ctx,
		`INSERT INTO photos (id, hash, original_name, mime, width, height, size_bytes, capture_date, caption, uploaded_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		pid, md.Hash, originalName, md.MIME, md.Width, md.Height, md.SizeBytes, nullIfEmpty(capture), uploaderID, now.Format(time.RFC3339))
	if err != nil {
		if isUnique(err, "photos.hash") {
			p, gerr := s.ByHash(ctx, md.Hash)
			if gerr != nil {
				return nil, false, err
			}
			return p, true, nil
		}
		return nil, false, err
	}
	p, err := s.Get(ctx, pid)
	return p, false, err
}

func (s *Store) ByHash(ctx context.Context, hash string) (*Photo, error) {
	var p Photo
	var deleted sql.NullString
	var capture sql.NullString
	var created string
	err := s.dbh.QueryRowContext(ctx,
		`SELECT id, hash, original_name, mime, width, height, size_bytes, capture_date, caption, uploaded_by, created_at, deleted_at
		   FROM photos WHERE hash = ?`, hash).
		Scan(&p.ID, &p.Hash, &p.OriginalName, &p.MIME, &p.Width, &p.Height, &p.SizeBytes,
			&capture, &p.Caption, &p.UploadedBy, &created, &deleted)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	finalizePhoto(&p, capture, deleted, created)
	return &p, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Photo, error) {
	return s.get(ctx, id, false)
}

// GetIncludingDeleted is used by the recovery UI (admin or uploader).
func (s *Store) GetIncludingDeleted(ctx context.Context, id string) (*Photo, error) {
	return s.get(ctx, id, true)
}

func (s *Store) get(ctx context.Context, id string, includeDeleted bool) (*Photo, error) {
	q := `SELECT p.id, p.hash, p.original_name, p.mime, p.width, p.height, p.size_bytes,
	             p.capture_date, p.caption, p.uploaded_by, u.display_name, p.created_at, p.deleted_at
	           FROM photos p JOIN users u ON u.id = p.uploaded_by WHERE p.id = ?`
	if !includeDeleted {
		q += ` AND p.deleted_at IS NULL`
	}
	var p Photo
	var capture, deleted sql.NullString
	var created string
	err := s.dbh.QueryRowContext(ctx, q, id).
		Scan(&p.ID, &p.Hash, &p.OriginalName, &p.MIME, &p.Width, &p.Height, &p.SizeBytes,
			&capture, &p.Caption, &p.UploadedBy, &p.UploaderName, &created, &deleted)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	finalizePhoto(&p, capture, deleted, created)
	return &p, nil
}

// ListParams controls listing/search.
type ListParams struct {
	Search   string // matches original_name or caption (LIKE, case-insensitive)
	AlbumID  string // restrict to photos in album
	Uploader string // restrict to uploader
	Deleted  bool   // list soft-deleted (recovery view)
	Limit    int
	Offset   int
}

func (s *Store) List(ctx context.Context, p ListParams) ([]Photo, int, error) {
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 100
	}
	where := []string{}
	args := []any{}
	if p.Deleted {
		where = append(where, "ph.deleted_at IS NOT NULL")
	} else {
		where = append(where, "ph.deleted_at IS NULL")
	}
	if p.Search != "" {
		like := "%" + strings.ToLower(p.Search) + "%"
		where = append(where, "(LOWER(ph.original_name) LIKE ? OR LOWER(ph.caption) LIKE ?)")
		args = append(args, like, like)
	}
	if p.AlbumID != "" {
		where = append(where, "EXISTS (SELECT 1 FROM album_photos ap WHERE ap.photo_id = ph.id AND ap.album_id = ?)")
		args = append(args, p.AlbumID)
	}
	if p.Uploader != "" {
		where = append(where, "ph.uploaded_by = ?")
		args = append(args, p.Uploader)
	}
	whereSQL := "WHERE " + strings.Join(where, " AND ")

	var total int
	countQ := `SELECT COUNT(*) FROM photos ph ` + whereSQL
	if err := s.dbh.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT ph.id, ph.hash, ph.original_name, ph.mime, ph.width, ph.height, ph.size_bytes,
	             ph.capture_date, ph.caption, ph.uploaded_by, u.display_name, ph.created_at, ph.deleted_at
	           FROM photos ph JOIN users u ON u.id = ph.uploaded_by
	           ` + whereSQL + `
	           ORDER BY COALESCE(ph.capture_date, ph.created_at) DESC, ph.created_at DESC
	           LIMIT ? OFFSET ?`
	rows, err := s.dbh.QueryContext(ctx, q, append(args, p.Limit, p.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Photo
	for rows.Next() {
		var p Photo
		var capture, deleted sql.NullString
		var created string
		if err := rows.Scan(&p.ID, &p.Hash, &p.OriginalName, &p.MIME, &p.Width, &p.Height, &p.SizeBytes,
			&capture, &p.Caption, &p.UploadedBy, &p.UploaderName, &created, &deleted); err != nil {
			return nil, 0, err
		}
		finalizePhoto(&p, capture, deleted, created)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Attach album membership in one query.
	if len(out) > 0 {
		ids := make([]string, len(out))
		for i, p := range out {
			ids[i] = p.ID
		}
		ph := strings.Trim(strings.Repeat("?,", len(ids)), ",")
		aq := `SELECT photo_id, album_id FROM album_photos WHERE photo_id IN (` + ph + `)`
		args := make([]any, len(ids))
		for i := range ids {
			args[i] = ids[i]
		}
		arows, err := s.dbh.QueryContext(ctx, aq, args...)
		if err != nil {
			return nil, 0, err
		}
		byID := make(map[string]*Photo, len(out))
		for i := range out {
			byID[out[i].ID] = &out[i]
		}
		for arows.Next() {
			var photoID, albumID string
			if err := arows.Scan(&photoID, &albumID); err != nil {
				arows.Close()
				return nil, 0, err
			}
			if p, ok := byID[photoID]; ok {
				p.Albums = append(p.Albums, albumID)
			}
		}
		arows.Close()
	}
	return out, total, nil
}

// UpdateMetadata edits caption and/or capture date (spec §5: "Allow
// users to edit captions and capture dates"). Empty values mean
// unchanged.
func (s *Store) UpdateMetadata(ctx context.Context, id, caption string, captureDate *time.Time) (*Photo, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	if caption != "" {
		if _, err := s.dbh.ExecContext(ctx, `UPDATE photos SET caption = ? WHERE id = ?`, caption, id); err != nil {
			return nil, err
		}
	}
	if captureDate != nil {
		v := nullIfEmpty(captureDate.UTC().Format(time.RFC3339))
		if _, err := s.dbh.ExecContext(ctx, `UPDATE photos SET capture_date = ? WHERE id = ?`, v, id); err != nil {
			return nil, err
		}
	}
	return s.Get(ctx, id)
}

// SoftDelete / Restore (spec §5: soft deletion and recovery).
func (s *Store) SoftDelete(ctx context.Context, id string) error {
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE photos SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Restore(ctx context.Context, id string) error {
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE photos SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func finalizePhoto(p *Photo, capture, deleted sql.NullString, created string) {
	if capture.Valid && capture.String != "" {
		if t, err := time.Parse(time.RFC3339, capture.String); err == nil {
			p.CaptureDate = &t
		}
	}
	if deleted.Valid && deleted.String != "" {
		if t, err := time.Parse(time.RFC3339, deleted.String); err == nil {
			p.DeletedAt = &t
		}
	}
	if t, err := time.Parse(time.RFC3339, created); err == nil {
		p.CreatedAt = t
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUnique(err error, tableCol string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") && strings.Contains(msg, tableCol)
}
