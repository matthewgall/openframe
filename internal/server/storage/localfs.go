package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LocalFS is the local filesystem storage backend.
//
// Layout under root:
//
//	root/originals/<sha256>
//	root/derivatives/<sha256>.<variant>
//
// Writes go to a temp file in the same directory, are verified against
// the expected content hash where one is known, then renamed into place
// (atomic on POSIX). Directories are 0700, files 0600 (spec §19:
// restrictive permissions).
type LocalFS struct {
	root string

	mu      sync.Mutex
	cache   atomic.Int64 // cached usage in bytes
	cacheAt atomic.Int64 // unixnano of last scan
}

var _ Storage = (*LocalFS)(nil)

// NewLocalFS opens (creating) the storage root.
func NewLocalFS(root string) (*LocalFS, error) {
	for _, d := range []string{
		root,
		filepath.Join(root, "originals"),
		filepath.Join(root, "derivatives"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("storage: create %s: %w", d, err)
		}
	}
	return &LocalFS{root: root}, nil
}

// Root returns the filesystem root (for diagnostics/backup tooling).
func (l *LocalFS) Root() string { return l.root }

func (l *LocalFS) path(key Key) (string, error) {
	k := string(key)
	if k == "" {
		return "", fmt.Errorf("storage: empty key")
	}
	if !strings.ContainsRune(k, '.') {
		// Bare sha256 hex: original.
		if len(k) != 64 || !isHex(k) {
			return "", fmt.Errorf("storage: invalid object key %q", k)
		}
		return filepath.Join(l.root, "originals", k), nil
	}
	parts := strings.SplitN(k, ".", 2)
	if len(parts[0]) != 64 || !isHex(parts[0]) {
		return "", fmt.Errorf("storage: invalid object key %q", k)
	}
	v := parts[1]
	for _, valid := range AllVariants {
		if v == valid {
			return filepath.Join(l.root, "derivatives", k), nil
		}
	}
	return "", fmt.Errorf("storage: invalid variant in key %q", k)
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// Open implements Storage.
func (l *LocalFS) Open(_ context.Context, key Key) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, err
	}
	return f, nil
}

// Size implements Storage.
func (l *LocalFS) Size(_ context.Context, key Key) (int64, error) {
	p, err := l.path(key)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return 0, err
	}
	return fi.Size(), nil
}

// Exists implements Storage.
func (l *LocalFS) Exists(_ context.Context, key Key) (bool, error) {
	p, err := l.path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Put implements Storage (create-if-absent with content verification).
func (l *LocalFS) Put(ctx context.Context, key Key, data io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := l.path(key)
	if err != nil {
		return err
	}
	alreadyExists := false
	if _, err := os.Stat(p); err == nil {
		alreadyExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Originals use bare-hash keys: the key IS the content hash, so the
	// written bytes must hash back to it (catches corruption in flight).
	// Derivatives are re-encoded, so their bytes legitimately differ
	// from the original and are not hash-verified here; a failed
	// generation leaves no file because we only rename on success.
	verifyOriginal := strings.IndexRune(string(key), '.') < 0

	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("storage: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	// Stream to the temp file while hashing. The temp file is renamed
	// into place only after verification, so an existing object is
	// never touched by a violating write.
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), data)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("storage: write %s: %w", key, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("storage: close %s: %w", key, err)
	}

	incoming := hex.EncodeToString(h.Sum(nil))
	if verifyOriginal && incoming != string(key) {
		return fmt.Errorf("storage: content hash mismatch for %s (got %s, want %s)", key, incoming, key)
	}
	if alreadyExists {
		// Existing object: keep it (for originals the incoming bytes
		// were verified against the key above, which is the same
		// content by construction).
		return nil
	}

	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("storage: commit %s: %w", key, err)
	}
	// Invalidate the usage cache.
	l.cache.Store(0)
	return nil
}

// usageCacheTTL bounds how stale TotalUsage may be.
const usageCacheTTL = 30 * time.Second

// TotalUsage implements Storage (30s cached bounded scan).
func (l *LocalFS) TotalUsage(_ context.Context) (int64, error) {
	if now := time.Now().UnixNano(); now-l.cacheAt.Load() < int64(usageCacheTTL) {
		if v := l.cache.Load(); v > 0 {
			return v, nil
		}
	}
	var total int64
	for _, sub := range []string{"originals", "derivatives"} {
		entries, err := os.ReadDir(filepath.Join(l.root, sub))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
				continue
			}
			if fi, err := e.Info(); err == nil {
				total += fi.Size()
			}
		}
	}
	l.cache.Store(total)
	l.cacheAt.Store(time.Now().UnixNano())
	return total, nil
}
