package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/server/devices"
	"github.com/matthewgall/openframe/internal/server/storage"
	migs "github.com/matthewgall/openframe/migrations"
)

// seedBenchmark builds a real server database + local storage with
// one device assigned to one album of n photos, each with its
// display + thumb derivatives stored AND recorded in
// photo_derivatives (the steady-state path the frame polls).
func seedBenchmark(tb testing.TB, n int) (*sql.DB, storage.Storage, devices.ManifestState) {
	t := tb
	t.Helper()
	ctx := context.Background()
	dbh, err := db.Open(ctx, t.TempDir()+"/bench.db", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	if _, err := db.MigrateFromFS(ctx, dbh, migs.FS(), "server"); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	stmts := []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO users (id, email, display_name, role, password_hash, created_at, updated_at) VALUES ('u1','u1@x','U','member','x',?,?)", []any{now, now}},
		{"INSERT INTO devices (id, name, created_at) VALUES ('dev1','Bench',?)", []any{now}},
		{"INSERT INTO albums (id, name, description, is_shared, created_by, created_at, updated_at, sort_order) VALUES ('alb1','Bench','',1,'u1',?,?,0)", []any{now, now}},
		{"INSERT INTO album_devices (album_id, device_id, assigned_at) VALUES ('alb1','dev1',?)", []any{now}},
	}
	for _, s := range stmts {
		if _, err := dbh.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatal(err)
		}
	}

	display := bytes.Repeat([]byte("d"), 120_000)
	thumb := bytes.Repeat([]byte("t"), 12_000)
	dSha := sha256.Sum256(display)
	tSha := sha256.Sum256(thumb)

	photoIns, _ := dbh.PrepareContext(ctx,
		`INSERT INTO photos (id, hash, original_name, mime, width, height, size_bytes, uploaded_by, created_at)
		 VALUES (?,?,?,?,1280,800,?, 'u1', ?)`)
	albumIns, _ := dbh.PrepareContext(ctx,
		"INSERT INTO album_photos (album_id, photo_id, sort_order, added_at) VALUES ('alb1',?,0,?)")
	derivIns, _ := dbh.PrepareContext(ctx,
		`INSERT INTO photo_derivatives (photo_hash, variant, sha256, size_bytes, generated_at)
		 VALUES (?,?,?,?,?)`)
	defer photoIns.Close()
	defer albumIns.Close()
	defer derivIns.Close()

	for i := 0; i < n; i++ {
		var buf bytes.Buffer
		buf.WriteString("photo-")
		buf.WriteString(itoa(i))
		sum := sha256.Sum256(buf.Bytes())
		hash := hex.EncodeToString(sum[:])
		id := hash[:8] + "-0000-0000-0000-" + hash[8:20]
		if _, err := photoIns.ExecContext(ctx, id, hash, "p.jpg", "image/jpeg", int64(len(hash)), now); err != nil {
			t.Fatal(err)
		}
		if _, err := albumIns.ExecContext(ctx, id, now); err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, storage.Key(hash+".display"), bytes.NewReader(display)); err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, storage.Key(hash+".thumb"), bytes.NewReader(thumb)); err != nil {
			t.Fatal(err)
		}
		if _, err := derivIns.ExecContext(ctx, hash, "display", hex.EncodeToString(dSha[:]), int64(len(display)), now); err != nil {
			t.Fatal(err)
		}
		if _, err := derivIns.ExecContext(ctx, hash, "thumb", hex.EncodeToString(tSha[:]), int64(len(thumb)), now); err != nil {
			t.Fatal(err)
		}
	}
	return dbh, store, devices.ManifestState{}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	d := ""
	for i > 0 {
		d = string(rune('0'+i%10)) + d
		i /= 10
	}
	return d
}

// BenchmarkBuildManifest/500 is the spec-scale home library: a frame
// polling its manifest every few minutes over 500 photos must stay
// comfortably under a second on Pi-class hardware.
func BenchmarkBuildManifest(b *testing.B) {
	for _, n := range []int{100, 500} {
		b.Run("photos_"+itoa(n), func(b *testing.B) {
			dbh, store, state := seedBenchmark(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := Build(b.Context(), dbh, store, "dev1", state, &devices.FrameConfig{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
