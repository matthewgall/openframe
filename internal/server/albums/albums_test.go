package albums

import (
	"context"
	"testing"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/migrations"
)

func newTest(t *testing.T) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	dbh, err := db.Open(ctx, t.TempDir()+"/test.db", false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	uid := "22222222-2222-2222-2222-222222222222"
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, role, password_hash, created_at, updated_at)
		 VALUES (?, 'a@example.com', 'A', 'member', 'x', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		uid); err != nil {
		t.Fatal(err)
	}
	return NewStore(dbh), uid
}

func insertPhoto(t *testing.T, s *Store, uid, hash string) string {
	t.Helper()
	ctx := context.Background()
	pid := "33333333-3333-3333-3333-" + hash[:12]
	if _, err := s.dbh.ExecContext(ctx,
		`INSERT INTO photos (id, hash, original_name, mime, width, height, size_bytes, uploaded_by, created_at)
		 VALUES (?, ?, 'x.jpg', 'image/jpeg', 100, 100, 10, ?, '2026-01-01T00:00:00Z')`,
		pid, hash, uid); err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestCreateListUpdate(t *testing.T) {
	s, uid := newTest(t)
	ctx := context.Background()

	a, err := s.Create(ctx, "  Holiday ", "  ", true, uid)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Name != "Holiday" {
		t.Fatalf("name not trimmed: %q", a.Name)
	}
	if !a.Shared {
		t.Fatal("shared flag lost")
	}
	if a.SortOrder != 0 {
		t.Fatalf("first album sort_order: %d", a.SortOrder)
	}

	// Second album appends.
	b, _ := s.Create(ctx, "Work", "", false, uid)
	if b.SortOrder != 1 {
		t.Fatalf("second album sort_order: %d", b.SortOrder)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list: %d", len(list))
	}

	// Update name/shared.
	shared := false
	updated, err := s.Update(ctx, a.ID, "Winter Holiday", "", &shared, -1)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Winter Holiday" || updated.Shared {
		t.Fatalf("update: %+v", updated)
	}

	// Bad names rejected.
	if _, err := s.Create(ctx, "", "", true, uid); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestAddRemovePhotoAndCover(t *testing.T) {
	s, uid := newTest(t)
	ctx := context.Background()
	a, _ := s.Create(ctx, "Album", "", true, uid)
	p1 := insertPhoto(t, s, uid, "aaaabbbbccccddddeeeeffff000011112222333344445555666677778888")
	p2 := insertPhoto(t, s, uid, "ffffeeee0000111122223333444455556666777788889999aaaabbbbccccdddd")

	if err := s.AddPhoto(ctx, a.ID, p1); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if err := s.AddPhoto(ctx, a.ID, p1); err != nil {
		t.Fatal(err)
	}
	// First photo becomes the cover automatically.
	got, _ := s.Get(ctx, a.ID)
	if got.PhotoCount != 1 {
		t.Fatalf("photo_count: %d", got.PhotoCount)
	}
	if got.CoverPhotoID != p1 {
		t.Fatalf("auto cover: %s", got.CoverPhotoID)
	}

	if err := s.AddPhoto(ctx, a.ID, p2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetCover(ctx, a.ID, p2); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, a.ID)
	if got.CoverPhotoID != p2 || got.CoverThumb == "" {
		t.Fatalf("cover: %+v", got)
	}

	// Cover must be a member of the album.
	p3 := insertPhoto(t, s, uid, "1212121212123434343456565656787878789090abab12121212121212121212")
	if _, err := s.SetCover(ctx, a.ID, p3); err == nil {
		t.Fatal("cover set to a non-member photo")
	}

	if err := s.RemovePhoto(ctx, a.ID, p1); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePhoto(ctx, a.ID, p1); err == nil {
		t.Fatal("double remove accepted")
	}
	ids, _ := s.Photos(ctx, a.ID)
	if len(ids) != 1 || ids[0] != p2 {
		t.Fatalf("photos: %v", ids)
	}
}

func TestSoftDeleteRestore(t *testing.T) {
	s, uid := newTest(t)
	ctx := context.Background()
	a, _ := s.Create(ctx, "Doomed", "", true, uid)

	if err := s.SoftDelete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, a.ID); err != ErrNotFound {
		t.Fatalf("deleted album still visible: %v", err)
	}
	inc, err := s.GetIncludingDeleted(ctx, a.ID)
	if err != nil || inc.DeletedAt == nil {
		t.Fatalf("including deleted: %v", err)
	}
	if err := s.Restore(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, a.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

func TestDeviceAssignment(t *testing.T) {
	s, uid := newTest(t)
	ctx := context.Background()
	a, _ := s.Create(ctx, "Frame Album", "", true, uid)

	dev := "44444444-4444-4444-4444-444444444444"
	if _, err := s.dbh.ExecContext(ctx,
		`INSERT INTO devices (id, name, created_at) VALUES (?, 'Pi 3B+', '2026-01-01T00:00:00Z')`, dev); err != nil {
		t.Fatal(err)
	}

	if err := s.AssignDevice(ctx, a.ID, dev); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if err := s.AssignDevice(ctx, a.ID, dev); err != nil {
		t.Fatal(err)
	}
	devs, err := s.Devices(ctx, a.ID)
	if err != nil || len(devs) != 1 || devs[0] != dev {
		t.Fatalf("devices: %v %v", devs, err)
	}
	if err := s.UnassignDevice(ctx, a.ID, dev); err != nil {
		t.Fatal(err)
	}
	devs, _ = s.Devices(ctx, a.ID)
	if len(devs) != 0 {
		t.Fatalf("unassign: %v", devs)
	}
}
