package photos

import (
	"context"
	"testing"
	"time"
)

func testMeta() Metadata {
	return Metadata{
		Hash:      "aa" + "bb" + "cc" + "dd" + "ee" + "ff" + "00" + "11" + "22" + "33" + "44" + "55" + "66" + "77" + "88" + "99",
		MIME:      MIMEJPEG,
		Width:     1600,
		Height:    1000,
		SizeBytes: 12345,
	}
}

func TestCreateAndGet(t *testing.T) {
	dbh, uid := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()

	p, dup, err := s.Create(ctx, testMeta(), uid, "beach.jpg")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if dup {
		t.Fatal("fresh upload reported as duplicate")
	}
	if p.Width != 1600 || p.MIME != MIMEJPEG || p.OriginalName != "beach.jpg" {
		t.Fatalf("unexpected photo: %+v", p)
	}

	got, err := s.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Hash != p.Hash {
		t.Fatal("hash mismatch")
	}
}

func TestDuplicateByHash(t *testing.T) {
	dbh, uid := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()

	meta := testMeta()
	first, dup, err := s.Create(ctx, meta, uid, "a.jpg")
	if err != nil || dup {
		t.Fatalf("first create: dup=%v err=%v", dup, err)
	}
	// Same content hash, different name/uploader context.
	second, dup, err := s.Create(ctx, meta, uid, "b.jpg")
	if err != nil {
		t.Fatalf("duplicate create: %v", err)
	}
	if !dup {
		t.Fatal("expected duplicate=true for identical hash")
	}
	if second.ID != first.ID {
		t.Fatal("duplicate should return the existing photo row")
	}
}

func TestUpdateMetadata(t *testing.T) {
	dbh, uid := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()
	p, _, _ := s.Create(ctx, testMeta(), uid, "a.jpg")

	capture := time.Date(2024, 7, 4, 12, 0, 0, 0, time.UTC)
	updated, err := s.UpdateMetadata(ctx, p.ID, "family day", &capture)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Caption != "family day" {
		t.Fatalf("caption: %q", updated.Caption)
	}
	if updated.CaptureDate == nil || !updated.CaptureDate.Equal(capture) {
		t.Fatalf("capture date: %v", updated.CaptureDate)
	}
}

func TestSoftDeleteAndRestore(t *testing.T) {
	dbh, uid := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()
	p, _, _ := s.Create(ctx, testMeta(), uid, "a.jpg")

	if err := s.SoftDelete(ctx, p.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	// Live Get must not see it.
	if _, err := s.Get(ctx, p.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after soft delete, got: %v", err)
	}
	// Recovery view sees it.
	inc, err := s.GetIncludingDeleted(ctx, p.ID)
	if err != nil {
		t.Fatalf("get including deleted: %v", err)
	}
	if inc.DeletedAt == nil {
		t.Fatal("deleted_at not set")
	}
	if err := s.Restore(ctx, p.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := s.Get(ctx, p.ID)
	if err != nil || got.DeletedAt != nil {
		t.Fatalf("restore failed: %v %+v", err, got)
	}
}

func TestListSearchAndDeletedFilter(t *testing.T) {
	dbh, uid := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()

	m1 := testMeta()
	m2 := testMeta()
	m2.Hash = "ff" + "ee" + "dd" + "cc" + "bb" + "aa" + "99" + "88" + "77" + "66" + "55" + "44" + "33" + "22" + "11"
	p1, _, _ := s.Create(ctx, m1, uid, "summer-vacation.jpg")
	p2, _, _ := s.Create(ctx, m2, uid, "work-meeting.png")
	if err := s.SoftDelete(ctx, p2.ID); err != nil {
		t.Fatal(err)
	}

	// Default: live photos only.
	ph, total, err := s.List(ctx, ListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(ph) != 1 || ph[0].ID != p1.ID {
		t.Fatalf("live list: total=%d len=%d", total, len(ph))
	}

	// Search by name.
	ph, total, _ = s.List(ctx, ListParams{Search: "summer"})
	if total != 1 || ph[0].OriginalName != "summer-vacation.jpg" {
		t.Fatalf("search: total=%d", total)
	}
	// Search by caption (case-insensitive).
	if _, err := s.UpdateMetadata(ctx, p1.ID, "SUMMER memories", nil); err != nil {
		t.Fatal(err)
	}
	ph, total, _ = s.List(ctx, ListParams{Search: "memories"})
	if total != 1 {
		t.Fatalf("caption search: total=%d", total)
	}

	// Deleted-only view.
	ph, total, _ = s.List(ctx, ListParams{Deleted: true})
	if total != 1 || ph[0].ID != p2.ID {
		t.Fatalf("deleted list: total=%d", total)
	}

	// Uploader filter.
	ph, total, _ = s.List(ctx, ListParams{Uploader: uid})
	if total != 1 {
		t.Fatalf("uploader filter: total=%d", total)
	}
}

func TestListAlbumFilter(t *testing.T) {
	dbh, uid := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()

	m1 := testMeta()
	m2 := testMeta()
	m2.Hash = "ff" + "ee" + "dd" + "cc" + "bb" + "aa" + "99" + "88" + "77" + "66" + "55" + "44" + "33" + "22" + "11"
	p1, _, _ := s.Create(ctx, m1, uid, "a.jpg")
	if _, _, err := s.Create(ctx, m2, uid, "b.jpg"); err != nil {
		t.Fatal(err)
	}

	albumID := "11111111-1111-1111-1111-111111111111"
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO albums (id, name, created_by, created_at, updated_at)
		 VALUES (?, 'Vacation', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		albumID, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO album_photos (album_id, photo_id, added_at) VALUES (?, ?, '2026-01-01T00:00:00Z')`,
		albumID, p1.ID); err != nil {
		t.Fatal(err)
	}

	ph, total, err := s.List(ctx, ListParams{AlbumID: albumID})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || ph[0].ID != p1.ID {
		t.Fatalf("album filter: total=%d", total)
	}
	if len(ph[0].Albums) != 1 || ph[0].Albums[0] != albumID {
		t.Fatalf("album membership not populated: %+v", ph[0].Albums)
	}
}

func TestByHash(t *testing.T) {
	dbh, uid := newTestDB(t)
	s := NewStore(dbh)
	ctx := context.Background()
	meta := testMeta()
	p, _, _ := s.Create(ctx, meta, uid, "a.jpg")

	got, err := s.ByHash(ctx, meta.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != p.ID {
		t.Fatal("by-hash returned wrong photo")
	}
}
