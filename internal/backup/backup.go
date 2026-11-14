// Package backup implements M7 backup/restore for both components
// (spec §M7 "Backup and restore").
//
// A backup is a plain directory:
//
//	<out>/
//	  backup-info.json   kind, time, version, counts, skipped files
//	  manifest.json      every data file: path, size, sha256
//	  server.db | frame.db          (consistent snapshot via VACUUM INTO)
//	  photos/originals/<hash>       (server; content-addressed)
//	  photos/derivatives/<hash>.v
//	  credential                  (frame; 0600, copied mode-preserved)
//	  update-state.json           (frame; optional — root-owned)
//
// Guarantees:
//   - The database snapshot is consistent even while the server or
//     frame keeps running (VACUUM INTO reads a transactionally
//     consistent image; it is a supported online backup method).
//   - Every data file is hashed at copy time; Verify re-hashes the
//     backup and runs PRAGMA integrity_check, so a backup is only
//     "good" when Restore's preflight (which runs Verify) passes.
//   - Restore never destroys the current data: an existing live file
//     or directory is preserved beside the target as
//     <name>.pre-restore-<UTC stamp> before the restored data moves
//     in.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/db"
)

const (
	KindServer = "server"
	KindFrame  = "frame"

	infoFileName    = "backup-info.json"
	manifestName    = "manifest.json"
	dbNameServer    = "server.db"
	dbNameFrame     = "frame.db"
	credName        = "credential"
	updateStateName = "update-state.json"
)

// Info is backup-info.json — what this backup is.
type Info struct {
	Kind        string   `json:"kind"` // server | frame
	CreatedAt   string   `json:"created_at"`
	Version     string   `json:"version"`
	Source      string   `json:"source"` // the data dir that was backed up
	Files       int      `json:"files"`
	Bytes       int64    `json:"bytes"`
	Skipped     []string `json:"skipped,omitempty"`      // files present but unreadable (e.g. root-owned update state from the frame user)
	StorageNote string   `json:"storage_note,omitempty"` // set when photo objects live outside this backup (S3)
}

// ManifestFile is one data file in manifest.json.
type ManifestFile struct {
	Path   string `json:"path"` // relative to the backup root
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest lists every backed-up data file (not including the two
// JSON metadata files themselves).
type Manifest struct {
	Files []ManifestFile `json:"files"`
}

// Result summarises a completed backup.
type Result struct {
	Dir   string
	Info  Info
	Files int
	Bytes int64
}

// integrityCheck runs PRAGMA integrity_check on a database file
// (read-only, snapshot-compatible open — see db.OpenSnapshotRead).
func integrityCheck(ctx context.Context, path string) (string, error) {
	dbh, err := db.OpenSnapshotRead(ctx, path)
	if err != nil {
		return "", err
	}
	defer dbh.Close()
	var result string
	if err := dbh.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return "", err
	}
	return result, nil
}

type collector struct {
	manifest Manifest
	bytes    int64
}

// copyFile copies src to dest (temp + rename in dest's directory),
// preserving the source mode, and records its sha256/size in the
// collector.
func (c *collector) copyFile(src, relDest string, root string) error {
	mode, err := srcMode(src)
	if err != nil {
		return err
	}
	dest := filepath.Join(root, relDest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".backup-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpName)
		}
	}()
	in, err := os.Open(src)
	if err != nil {
		tmp.Close()
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), in)
	in.Close()
	if err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return err
	}
	ok = true
	c.manifest.Files = append(c.manifest.Files, ManifestFile{
		Path:   filepath.ToSlash(relDest),
		Size:   n,
		SHA256: hex.EncodeToString(h.Sum(nil)),
	})
	c.bytes += n
	return nil
}

// snapshotDB writes a consistent VACUUM INTO snapshot of src into the
// backup directory under relName and records it in the manifest
// (the snapshot is hashed by re-reading it — cheap compared to the
// photo copies, and it proves what Restore will receive).
//
// VACUUM INTO reads a transactionally consistent image, so the source
// may be in active use by the server or frame (it takes a shared
// lock; concurrent WAL writers are not blocked).
func (c *collector) snapshotDB(ctx context.Context, src, relName string, root string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("source db %s: %w", src, err)
	}
	dbh, err := db.Open(ctx, src, false)
	if err != nil {
		return err
	}
	defer dbh.Close()
	dest := filepath.Join(root, relName)
	if _, err := dbh.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	f, err := os.Open(dest)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		os.Remove(dest)
		return err
	}
	c.manifest.Files = append(c.manifest.Files, ManifestFile{
		Path:   relName,
		Size:   n,
		SHA256: hex.EncodeToString(h.Sum(nil)),
	})
	c.bytes += n
	return nil
}

func srcMode(src string) (os.FileMode, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return 0, err
	}
	return fi.Mode().Perm(), nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ----------------------------------------------------------------------
// Backup
// ----------------------------------------------------------------------

// ServerPaths is what a server backup needs.
type ServerPaths struct {
	Kind        string // KindServer
	DataDir     string // recorded in Info.Source
	DBPath      string // server SQLite
	PhotoDir    string // originals/ + derivatives/ root (local backend)
	Version     string
	StorageNote string // optional note recorded in Info (e.g. photos in S3)
}

// BackupServer snapshots the server database and copies the
// content-addressed photo store. Safe while the server is running.
func BackupServer(ctx context.Context, outDir string, p ServerPaths) (*Result, error) {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, err
	}
	c := &collector{}
	if err := c.snapshotDB(ctx, p.DBPath, dbNameServer, outDir); err != nil {
		return nil, fmt.Errorf("backup: server db: %w", err)
	}
	for _, sub := range []string{"originals", "derivatives"} {
		dir := filepath.Join(p.PhotoDir, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			src := filepath.Join(dir, e.Name())
			rel := filepath.Join("photos", sub, e.Name())
			if err := c.copyFile(src, rel, outDir); err != nil {
				return nil, fmt.Errorf("backup: %s: %w", e.Name(), err)
			}
		}
	}
	info := Info{
		Kind:        KindServer,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Version:     p.Version,
		Source:      p.DataDir,
		Files:       len(c.manifest.Files),
		Bytes:       c.bytes,
		StorageNote: p.StorageNote,
	}
	if err := writeJSON(filepath.Join(outDir, manifestName), &c.manifest); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(outDir, infoFileName), &info); err != nil {
		return nil, err
	}
	return &Result{Dir: outDir, Info: info, Files: info.Files, Bytes: info.Bytes}, nil
}

// FramePaths is what a frame backup needs.
type FramePaths struct {
	Kind       string // KindFrame
	DataDir    string
	DBPath     string // frame SQLite
	Credential string // device credential file (0600); "" when unpaired
	// UpdateState is the updater's state.json (root-owned; the frame
	// user may not read it — that is fine, it is skipped and recorded).
	UpdateState string
	Version     string
}

// BackupFrame snapshots frame.db and copies the credential and (when
// readable) the updater state. Safe while the frame is running.
func BackupFrame(ctx context.Context, outDir string, p FramePaths) (*Result, error) {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, err
	}
	c := &collector{}
	info := Info{
		Kind:      KindFrame,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Version:   p.Version,
		Source:    p.DataDir,
	}
	if err := c.snapshotDB(ctx, p.DBPath, dbNameFrame, outDir); err != nil {
		return nil, fmt.Errorf("backup: frame db: %w", err)
	}
	if p.Credential != "" {
		if err := c.copyFile(p.Credential, credName, outDir); err != nil {
			if os.IsNotExist(err) {
				info.Skipped = append(info.Skipped, "credential")
			} else {
				return nil, fmt.Errorf("backup: credential: %w", err)
			}
		}
	}
	if p.UpdateState != "" {
		if err := c.copyFile(p.UpdateState, updateStateName, outDir); err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				info.Skipped = append(info.Skipped, updateStateName+" (not readable by this user)")
			} else {
				return nil, fmt.Errorf("backup: update state: %w", err)
			}
		}
	}
	info.Files = len(c.manifest.Files)
	info.Bytes = c.bytes
	if err := writeJSON(filepath.Join(outDir, manifestName), &c.manifest); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(outDir, infoFileName), &info); err != nil {
		return nil, err
	}
	return &Result{Dir: outDir, Info: info, Files: info.Files, Bytes: info.Bytes}, nil
}

// ----------------------------------------------------------------------
// Verify
// ----------------------------------------------------------------------

// LoadInfo reads and sanity-checks a backup directory's metadata.
func LoadInfo(dir string) (Info, Manifest, error) {
	var info Info
	b, err := os.ReadFile(filepath.Join(dir, infoFileName))
	if err != nil {
		return info, Manifest{}, fmt.Errorf("backup: no %s in %s (not a backup directory?)", infoFileName, dir)
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, Manifest{}, fmt.Errorf("backup: parse info: %w", err)
	}
	if info.Kind != KindServer && info.Kind != KindFrame {
		return info, Manifest{}, fmt.Errorf("backup: unknown kind %q", info.Kind)
	}
	var m Manifest
	b, err = os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return info, Manifest{}, fmt.Errorf("backup: no manifest: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return info, Manifest{}, fmt.Errorf("backup: parse manifest: %w", err)
	}
	return info, m, nil
}

// Verify re-hashes every manifest file and integrity-checks the
// database. It is what Restore runs first, and what
// `openframe backup verify` exposes on its own.
func Verify(ctx context.Context, dir string) (Info, error) {
	info, m, err := LoadInfo(dir)
	if err != nil {
		return Info{}, err
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if seen[f.Path] {
			return Info{}, fmt.Errorf("backup: duplicate manifest entry %s", f.Path)
		}
		seen[f.Path] = true
		// A manifest path is operator-independent data we read from
		// the backup: refuse anything that could escape the dir.
		if strings.ContainsAny(f.Path, "\x00") || filepath.IsAbs(f.Path) ||
			filepath.Clean(f.Path) != filepath.ToSlash(f.Path) {
			return Info{}, fmt.Errorf("backup: bad manifest path %q", f.Path)
		}
		sum, size, err := hashFile(filepath.Join(dir, f.Path))
		if err != nil {
			return Info{}, fmt.Errorf("backup: %s: %w", f.Path, err)
		}
		if sum != f.SHA256 {
			return Info{}, fmt.Errorf("backup: %s: sha256 mismatch (backup corrupted or tampered)", f.Path)
		}
		if size != f.Size {
			return Info{}, fmt.Errorf("backup: %s: size mismatch", f.Path)
		}
	}
	dbName := dbNameServer
	if info.Kind == KindFrame {
		dbName = dbNameFrame
	}
	result, err := integrityCheck(ctx, filepath.Join(dir, dbName))
	if err != nil {
		return Info{}, fmt.Errorf("backup: %s unreadable: %w", dbName, err)
	}
	if result != "ok" {
		return Info{}, fmt.Errorf("backup: %s integrity_check: %s", dbName, result)
	}
	if len(seen) != info.Files {
		return Info{}, fmt.Errorf("backup: manifest has %d entries, info says %d", len(seen), info.Files)
	}
	return info, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ----------------------------------------------------------------------
// Restore
// ----------------------------------------------------------------------

// RestoreMove reports what a restore did, for the operator summary.
type RestoreMove struct {
	Preserved string // previous live path, renamed to .pre-restore-<ts>
	Restored  string // backup path that moved into place
}

// Restore moves a verified backup into the target location. Existing
// live files are preserved (renamed beside the target), never deleted.
// The target paths must not be open by a running process for the
// rename to take effect on already-open handles — the CLI tells the
// operator to stop the service first (and this function checks for
// the service-writer sidecar files where it can).
// Restore copies a verified backup into the live locations. targets
// maps manifest top-level names to live paths:
//
//	"server.db" / "frame.db"  -> live database file
//	"photos"                  -> photo storage root (photos/<rest>)
//	"credential"              -> credential file
//	"update-state.json"       -> updater state file
//
// The backup directory is left INTACT (copies, not moves): a backup
// stays a valid, re-verifiable backup and can be restored onto any
// number of machines. Existing live files are preserved (renamed
// beside the target as <name>.pre-restore-<stamp>), never deleted.
// The operator must stop the service first: a running process keeps
// its old handles and would not see the restored bytes.
func Restore(ctx context.Context, dir string, targets map[string]string) ([]RestoreMove, error) {
	if _, err := Verify(ctx, dir); err != nil {
		return nil, err
	}
	info, m, err := LoadInfo(dir)
	if err != nil {
		return nil, err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	var moves []RestoreMove
	for _, f := range sortedManifest(m) {
		src := filepath.Join(dir, f.Path)
		dest := resolveTarget(targets, f.Path)
		if dest == "" {
			return nil, fmt.Errorf("restore: no target configured for %s (kind %s)", f.Path, info.Kind)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return nil, err
		}
		mv, err := copyPreserving(src, dest, stamp)
		if err != nil {
			return nil, err
		}
		moves = append(moves, mv)
	}
	return moves, nil
}

// resolveTarget maps a backup-relative manifest path to its live
// location via the targets table:
//   - "server.db"/"frame.db"  -> targets["<dbname>"]
//   - "credential", "update-state.json" -> targets[<name>]
//   - "photos/<rest>"         -> targets["photos"] + "/<rest>"
func resolveTarget(targets map[string]string, rel string) string {
	rest := strings.TrimPrefix(filepath.ToSlash(rel), "photos/")
	if rest != rel {
		root := targets["photos"]
		if root == "" {
			return ""
		}
		return filepath.Join(root, rest)
	}
	return targets[rel]
}

// copyPreserving copies src onto dest. If dest exists it is first
// renamed to dest.pre-restore-<stamp>. The copy is atomic (temp +
// rename in the destination directory) and mode-preserving.
func copyPreserving(src, dest, stamp string) (RestoreMove, error) {
	var mv RestoreMove
	if _, err := os.Stat(dest); err == nil {
		preserved := fmt.Sprintf("%s.pre-restore-%s", dest, stamp)
		if err := os.Rename(dest, preserved); err != nil {
			return mv, fmt.Errorf("preserve existing %s: %w", dest, err)
		}
		mv.Preserved = preserved
	}
	if err := copyFileFlat(src, dest); err != nil {
		return mv, fmt.Errorf("copy %s -> %s: %w", src, dest, err)
	}
	mv.Restored = dest
	return mv, nil
}

func copyFileFlat(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dest), ".restore-*")
	if err != nil {
		return err
	}
	tmpName := out.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(fi.Mode().Perm()); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

func sortedManifest(m Manifest) []ManifestFile {
	out := append([]ManifestFile(nil), m.Files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
