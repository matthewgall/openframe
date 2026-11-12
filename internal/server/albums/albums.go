// Package albums implements the album domain (spec §5): create/edit,
// membership (a photo in many albums without duplicating the stored
// object), shared/private visibility, covers, ordering, soft delete.
package albums

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/id"
)

var (
	ErrNotFound = errors.New("albums: not found")
	ErrName     = errors.New("albums: invalid name")
)

// Album is the domain record.
type Album struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Shared       bool       `json:"shared"`
	CoverPhotoID string     `json:"cover_photo_id,omitempty"`
	CoverThumb   string     `json:"cover_thumb_url,omitempty"`
	PhotoCount   int        `json:"photo_count"`
	SortOrder    int        `json:"sort_order"`
	CreatedBy    string     `json:"created_by"`
	CreatorName  string     `json:"creator_name,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
}

// Store persists albums.
type Store struct{ dbh *sql.DB }

func NewStore(dbh *sql.DB) *Store { return &Store{dbh: dbh} }

// Create inserts an album.
func (s *Store) Create(ctx context.Context, name, description string, shared bool, creatorID string) (*Album, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return nil, ErrName
	}
	aid, err := id.NewUUID()
	if err != nil {
		return nil, err
	}
	// New albums append after existing ones.
	var maxOrder int
	_ = s.dbh.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort_order), -1) FROM albums WHERE deleted_at IS NULL`).Scan(&maxOrder)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.dbh.ExecContext(ctx,
		`INSERT INTO albums (id, name, description, is_shared, sort_order, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		aid, name, description, sharedInt(shared), maxOrder+1, creatorID, now, now)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, aid)
}

func (s *Store) Get(ctx context.Context, id string) (*Album, error) {
	return s.get(ctx, id, false)
}

// GetIncludingDeleted for the recovery view.
func (s *Store) GetIncludingDeleted(ctx context.Context, id string) (*Album, error) {
	return s.get(ctx, id, true)
}

func (s *Store) get(ctx context.Context, id string, includeDeleted bool) (*Album, error) {
	q := `SELECT a.id, a.name, a.description, a.is_shared, a.cover_photo_id, a.sort_order,
	             a.created_by, u.display_name, a.created_at, a.updated_at, a.deleted_at,
	             (SELECT COUNT(*) FROM album_photos ap WHERE ap.album_id = a.id)
	           FROM albums a JOIN users u ON u.id = a.created_by WHERE a.id = ?`
	if !includeDeleted {
		q += ` AND a.deleted_at IS NULL`
	}
	var a Album
	var shared, photoCount int
	var cover sql.NullString
	var deleted sql.NullString
	var created, updated string
	err := s.dbh.QueryRowContext(ctx, q, id).
		Scan(&a.ID, &a.Name, &a.Description, &shared, &cover, &a.SortOrder,
			&a.CreatedBy, &a.CreatorName, &created, &updated, &deleted, &photoCount)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Shared = shared == 1
	a.PhotoCount = photoCount
	if cover.Valid && cover.String != "" {
		a.CoverPhotoID = cover.String
		a.CoverThumb = coverThumbURL(cover.String)
	}
	a.CreatedAt = parseTS(created)
	a.UpdatedAt = parseTS(updated)
	if deleted.Valid && deleted.String != "" {
		t := parseTS(deleted.String)
		a.DeletedAt = &t
	}
	return &a, nil
}

// List returns live albums ordered by sort_order, created_at.
func (s *Store) List(ctx context.Context) ([]Album, error) {
	rows, err := s.dbh.QueryContext(ctx,
		`SELECT a.id, a.name, a.description, a.is_shared, a.cover_photo_id, a.sort_order,
		        a.created_by, u.display_name, a.created_at, a.updated_at, a.deleted_at,
		        (SELECT COUNT(*) FROM album_photos ap WHERE ap.album_id = a.id)
	           FROM albums a JOIN users u ON u.id = a.created_by
	          WHERE a.deleted_at IS NULL
	          ORDER BY a.sort_order, a.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Album
	for rows.Next() {
		var a Album
		var shared, photoCount int
		var cover, deleted sql.NullString
		var created, updated string
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &shared, &cover, &a.SortOrder,
			&a.CreatedBy, &a.CreatorName, &created, &updated, &deleted, &photoCount); err != nil {
			return nil, err
		}
		a.Shared = shared == 1
		a.PhotoCount = photoCount
		if cover.Valid && cover.String != "" {
			a.CoverPhotoID = cover.String
			a.CoverThumb = coverThumbURL(cover.String)
		}
		a.CreatedAt = parseTS(created)
		a.UpdatedAt = parseTS(updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetPriority marks (or unmarks) a collection for priority delivery
// (spec §9: "Administrators may mark a photograph or collection as
// priority").
func (s *Store) SetPriority(ctx context.Context, id string, priority bool) error {
	var n int
	if err := s.dbh.QueryRowContext(ctx, "SELECT COUNT(*) FROM albums WHERE id = ?", id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	v := 0
	if priority {
		v = 1
	}
	_, err := s.dbh.ExecContext(ctx, "UPDATE albums SET priority = ? WHERE id = ?", v, id)
	return err
}

// Update mutates name/description/shared/sort_order (empty/zero =
// unchanged for strings; sortOrder < 0 = unchanged).
func (s *Store) Update(ctx context.Context, id, name, description string, shared *bool, sortOrder int) (*Album, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if name != "" {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 200 {
			return nil, ErrName
		}
	}
	if shared == nil {
		shared = &a.Shared
	}
	if sortOrder < 0 {
		sortOrder = a.SortOrder
	}
	if _, err := s.dbh.ExecContext(ctx,
		`UPDATE albums SET name = ?, description = ?, is_shared = ?, sort_order = ?, updated_at = ?
		  WHERE id = ?`,
		nameOr(name, a.Name), descOr(description, a.Description),
		sharedInt(*shared), sortOrder, time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// SetCover sets the cover photo (validated to exist in the album).
func (s *Store) SetCover(ctx context.Context, albumID, photoID string) (*Album, error) {
	var n int
	err := s.dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM album_photos WHERE album_id = ? AND photo_id = ?`,
		albumID, photoID).Scan(&n)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("albums: cover photo is not in the album")
	}
	if _, err := s.dbh.ExecContext(ctx,
		`UPDATE albums SET cover_photo_id = ?, updated_at = ? WHERE id = ?`,
		photoID, time.Now().UTC().Format(time.RFC3339), albumID); err != nil {
		return nil, err
	}
	return s.Get(ctx, albumID)
}

// AddPhoto adds a photo to the album (membership; the stored original
// is shared, never copied). Idempotent.
func (s *Store) AddPhoto(ctx context.Context, albumID, photoID string) error {
	if _, err := s.dbh.ExecContext(ctx,
		`INSERT OR IGNORE INTO album_photos (album_id, photo_id, sort_order, added_at)
		 SELECT a.id, ?, (SELECT COALESCE(MAX(sort_order), -1) + 1 FROM album_photos WHERE album_id = a.id), ?
		 FROM albums a WHERE a.id = ? AND a.deleted_at IS NULL`,
		photoID, time.Now().UTC().Format(time.RFC3339), albumID); err != nil {
		return err
	}
	var n int
	if err := s.dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM album_photos WHERE album_id = ?`, albumID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	// If the album has no cover yet, use the first photo.
	if _, err := s.dbh.ExecContext(ctx,
		`UPDATE albums SET cover_photo_id = ? WHERE id = ? AND (cover_photo_id IS NULL OR cover_photo_id = '')`,
		photoID, albumID); err != nil {
		return err
	}
	return nil
}

// RemovePhoto removes a photo from the album.
func (s *Store) RemovePhoto(ctx context.Context, albumID, photoID string) error {
	res, err := s.dbh.ExecContext(ctx,
		`DELETE FROM album_photos WHERE album_id = ? AND photo_id = ?`, albumID, photoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("albums: photo not in album")
	}
	return nil
}

// Photos lists photo IDs in an album (ordered).
func (s *Store) Photos(ctx context.Context, albumID string) ([]string, error) {
	rows, err := s.dbh.QueryContext(ctx,
		`SELECT photo_id FROM album_photos WHERE album_id = ? ORDER BY sort_order, added_at`, albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AssignDevice / UnassignDevice (album -> frame assignment; the frame
// itself is M5, the assignment data model is M2 per spec §5).
func (s *Store) AssignDevice(ctx context.Context, albumID, deviceID string) error {
	if _, err := s.dbh.ExecContext(ctx,
		`INSERT OR IGNORE INTO album_devices (album_id, device_id, assigned_at) VALUES (?, ?, ?)`,
		albumID, deviceID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	var n int
	if err := s.dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM album_devices WHERE album_id = ?`, albumID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UnassignDevice(ctx context.Context, albumID, deviceID string) error {
	_, err := s.dbh.ExecContext(ctx,
		`DELETE FROM album_devices WHERE album_id = ? AND device_id = ?`, albumID, deviceID)
	return err
}

// Devices lists device IDs assigned to an album.
func (s *Store) Devices(ctx context.Context, albumID string) ([]string, error) {
	rows, err := s.dbh.QueryContext(ctx,
		`SELECT device_id FROM album_devices WHERE album_id = ? ORDER BY assigned_at`, albumID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SoftDelete / Restore.
func (s *Store) SoftDelete(ctx context.Context, id string) error {
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE albums SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
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
		`UPDATE albums SET deleted_at = NULL WHERE id = ? AND deleted_at IS NOT NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// coverThumbURL renders the authenticated thumbnail URL for a cover
// photo (the frame/browsers fetch it with the session cookie).
func coverThumbURL(photoID string) string {
	return fmt.Sprintf("/api/v1/photos/%s/file?variant=thumb", photoID)
}

func sharedInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nameOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func descOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
