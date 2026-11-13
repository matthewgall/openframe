package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/matthewgall/openframe/internal/db"
	migs "github.com/matthewgall/openframe/migrations"
)

// mkServerData builds a small but REAL server data dir: a migrated
// SQLite DB with a known row and a content-addressed photo store.
func mkServerData(t *testing.T) (dataDir, dbPath, photoDir string) {
	t.Helper()
	ctx := context.Background()
	dataDir = t.TempDir()
	serverDir := filepath.Join(dataDir, "server")
	dbPath = filepath.Join(serverDir, "server.db")
	photoDir = filepath.Join(serverDir, "photos")

	dbh, err := db.Open(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migs.FS(), "server"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('backup_test', 'roundtrip-123')"); err != nil {
		t.Fatal(err)
	}
	dbh.Close()

	for _, sub := range []string{"originals", "derivatives"} {
		if err := os.MkdirAll(filepath.Join(photoDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Content-addressed names (64 hex).
	if err := os.WriteFile(filepath.Join(photoDir, "originals", "aa"+repeat("0", 62)), []byte("original-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(photoDir, "derivatives", "bb"+repeat("0", 61)+".display"), []byte("display-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dataDir, dbPath, photoDir
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func queryMeta(ctx context.Context, dbPath, key string) (string, error) {
	dbh, err := db.OpenSnapshotRead(ctx, dbPath)
	if err != nil {
		return "", err
	}
	defer dbh.Close()
	var v string
	err = dbh.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	return v, err
}

// TestServerBackupRestoreCycle is the M7 server recovery test:
// backup a live data dir, destroy the live data, restore, and verify
// both the database contents and the photo bytes round-trip intact.
func TestServerBackupRestoreCycle(t *testing.T) {
	ctx := context.Background()
	dataDir, dbPath, photoDir := mkServerData(t)
	out := t.TempDir() + "/server-backup"

	if _, err := BackupServer(ctx, out, ServerPaths{
		Kind: KindServer, DataDir: dataDir, DBPath: dbPath, PhotoDir: photoDir, Version: "test",
	}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	info, err := Verify(ctx, out)
	if err != nil {
		t.Fatalf("verify fresh backup: %v", err)
	}
	if info.Kind != KindServer || info.Files != 3 {
		t.Fatalf("info = %+v (want kind server, 3 files)", info)
	}

	// --- Destroy the live data (simulated disk loss / corruption). ---
	if err := os.RemoveAll(filepath.Dir(dbPath) + "/server.db"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(photoDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("db still present after destruction")
	}

	// --- Restore. ---
	targets := map[string]string{
		"server.db": dbPath,
		"photos":    photoDir,
	}
	moves, err := Restore(ctx, out, targets)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(moves) != 3 {
		t.Fatalf("moves = %d, want 3", len(moves))
	}

	// --- Verify the round-trip. ---
	v, err := queryMeta(ctx, dbPath, "backup_test")
	if err != nil {
		t.Fatalf("restored db unreadable: %v", err)
	}
	if v != "roundtrip-123" {
		t.Fatalf("meta value = %q, want roundtrip-123", v)
	}
	orig, err := os.ReadFile(filepath.Join(photoDir, "originals", "aa"+repeat("0", 62)))
	if err != nil || string(orig) != "original-bytes" {
		t.Fatalf("original photo = %q, %v", orig, err)
	}
	deriv, err := os.ReadFile(filepath.Join(photoDir, "derivatives", "bb"+repeat("0", 61)+".display"))
	if err != nil || string(deriv) != "display-bytes" {
		t.Fatalf("derivative photo = %q, %v", deriv, err)
	}
}

// TestRestorePreservesExistingLiveData verifies that restoring over a
// non-empty live location preserves the old data beside the target
// (never deletes it) — the M7 guarantee that a bad restore is
// recoverable.
func TestRestorePreservesExistingLiveData(t *testing.T) {
	ctx := context.Background()
	dataDir, dbPath, photoDir := mkServerData(t)
	out := t.TempDir() + "/b"
	if _, err := BackupServer(ctx, out, ServerPaths{
		Kind: KindServer, DataDir: dataDir, DBPath: dbPath, PhotoDir: photoDir, Version: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// Now the live db gains a NEW row that is NOT in the backup.
	dbh, err := db.Open(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	dbh.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('post_backup', 'newer')")
	dbh.Close()

	targets := map[string]string{"server.db": dbPath, "photos": photoDir}
	moves, err := Restore(ctx, out, targets)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	// Exactly one move preserved the existing live db.
	preserved := 0
	for _, mv := range moves {
		if mv.Preserved != "" {
			preserved++
		}
	}
	if preserved == 0 {
		t.Fatalf("no live data preserved: %+v", moves)
	}
	// The pre-restore file exists alongside.
	found := false
	entries, _ := os.ReadDir(filepath.Dir(dbPath))
	for _, e := range entries {
		if filepath.Base(dbPath) == "" || e.Name() == filepath.Base(dbPath) {
			continue
		}
		if len(e.Name()) > len(filepath.Base(dbPath)) &&
			filepath.Base(dbPath)+".pre-restore-" == e.Name()[:len(filepath.Base(dbPath))+len(".pre-restore-")] {
			found = true
		}
	}
	if !found {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no .pre-restore-* file in %s: %v", filepath.Dir(dbPath), names)
	}
	// The restored (backup) db is now live and has the backup row.
	if v, err := queryMeta(ctx, dbPath, "backup_test"); err != nil || v != "roundtrip-123" {
		t.Fatalf("live db after restore = %q, %v", v, err)
	}
}

// TestVerifyDetectsTampering proves Verify rejects a backup whose
// bytes were changed after the fact (the restore preflight gate).
func TestVerifyDetectsTampering(t *testing.T) {
	ctx := context.Background()
	dataDir, dbPath, photoDir := mkServerData(t)
	out := t.TempDir() + "/b"
	if _, err := BackupServer(ctx, out, ServerPaths{
		Kind: KindServer, DataDir: dataDir, DBPath: dbPath, PhotoDir: photoDir, Version: "test",
	}); err != nil {
		t.Fatal(err)
	}
	// Corrupt one byte of a backed-up photo.
	target := filepath.Join(out, "photos", "originals", "aa"+repeat("0", 62))
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if err := os.WriteFile(target, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, out); err == nil {
		t.Fatal("Verify passed on a tampered backup")
	}
	// A restore from the tampered backup must refuse.
	if _, err := Restore(ctx, out, map[string]string{"server.db": t.TempDir() + "/x.db", "photos": t.TempDir()}); err == nil {
		t.Fatal("Restore accepted a tampered backup")
	}
}

// TestFrameBackupRestoreCycle covers the frame side: frame.db, the
// 0600 credential (mode must survive the round-trip), and an
// unreadable/absent update state (skipped, recorded, non-fatal).
func TestFrameBackupRestoreCycle(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	frameDir := filepath.Join(dataDir, "frame")
	updatesDir := filepath.Join(dataDir, "updates")
	dbPath := filepath.Join(frameDir, "frame.db")
	credPath := filepath.Join(frameDir, "credential")

	dbh, err := db.Open(ctx, dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.MigrateFromFS(ctx, dbh, migs.FS(), "frame"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.ExecContext(ctx, "INSERT INTO agent_state (key, value, updated_at) VALUES ('device_id', 'abc', 'now')"); err != nil {
		t.Fatal(err)
	}
	dbh.Close()
	if err := os.WriteFile(credPath, []byte(`{"device_id":"abc","token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// No update state file (simulates root-owned / absent).

	out := t.TempDir() + "/fb"
	res, err := BackupFrame(ctx, out, FramePaths{
		Kind: KindFrame, DataDir: dataDir, DBPath: dbPath, Credential: credPath,
		UpdateState: filepath.Join(updatesDir, "state.json"), Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Info.Skipped) != 1 {
		t.Fatalf("skipped = %v, want the update state", res.Info.Skipped)
	}
	if _, err := Verify(ctx, out); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Destroy and restore.
	os.RemoveAll(frameDir)
	targets := map[string]string{
		"frame.db":          dbPath,
		"credential":        credPath,
		"update-state.json": filepath.Join(updatesDir, "state.json"),
	}
	if _, err := Restore(ctx, out, targets); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// Credential mode must be 0600 after restore.
	fi, err := os.Stat(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %o, want 600", fi.Mode().Perm())
	}
	// Frame db round-trips.
	dbh2, err := db.OpenSnapshotRead(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var v string
	dbh2.QueryRowContext(ctx, "SELECT value FROM agent_state WHERE key = 'device_id'").Scan(&v)
	dbh2.Close()
	if v != "abc" {
		t.Fatalf("device_id = %q, want abc", v)
	}
}

// TestVerifyRejectsBadKind ensures a hand-made directory is refused.
func TestVerifyRejectsBadKind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(dir, infoFileName), Info{Kind: "rogue"}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, manifestName), &Manifest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, dir); err == nil {
		t.Fatal("Verify accepted an unknown kind")
	}
}
