// Package cache is the frame's content-addressed photo cache (spec §8).
//
// Layout under the frame's photo root:
//
//	derivatives/<hash>.<variant>   display and thumb images
//	derivatives/<hash>.<variant>.part   in-progress download (never served)
//
// The frame never stores originals; it keeps the derivative variants
// the renderer needs. Downloads are written to a .part file, hash
// verified and atomically committed (rename), so a crash or a full
// disk can never leave a corrupt file at a cache path. GC only ever
// removes files the caller says are no longer referenced by any
// active, staging or queued collection (spec §8: "Garbage collection
// must never remove photographs referenced by the active collection").
package cache

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Variants the frame caches. The server may add more in a later
// release; unknown variants are simply not requested (the manifest
// only advertises what this build caches).
const (
	VariantDisplay = "display"
	VariantThumb   = "thumb"
)

// KnownVariants lists every variant this build can cache.
var KnownVariants = []string{VariantDisplay, VariantThumb}

func validVariant(v string) bool {
	for _, k := range KnownVariants {
		if v == k {
			return true
		}
	}
	return false
}

// Store is a content-addressed derivative cache on the local
// filesystem.
type Store struct {
	dir          string // photo root; derivatives live in dir/derivatives
	quotaBytes   int64  // 0 = free-space heuristic (keep >= minFreeBytes)
	minFreeBytes int64
}

// minFreeBytes is the free-space floor when no explicit quota is set
// (spec §8: "Support storage quotas and disk-space monitoring"; 1 GiB
// keeps the frame away from a full SD card).
const minFreeBytes = int64(1 << 30)

// New opens (creating if needed) a cache store rooted at root.
// quotaBytes <= 0 selects the free-space heuristic.
func New(root string, quotaBytes int64) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("cache: empty root")
	}
	dir := filepath.Join(root, "derivatives")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cache: create %s: %w", dir, err)
	}
	if quotaBytes < 0 {
		quotaBytes = 0
	}
	return &Store{dir: dir, quotaBytes: quotaBytes}, nil
}

// Dir returns the derivatives directory.
func (s *Store) Dir() string { return s.dir }

// Path is the cache path of a committed file.
func (s *Store) Path(hash, variant string) string {
	return filepath.Join(s.dir, hash+"."+variant)
}

// PartPath is the in-progress download path for a file.
func (s *Store) PartPath(hash, variant string) string {
	return s.Path(hash, variant) + ".part"
}

// Exists reports whether the file is committed (not .part).
func (s *Store) Exists(hash, variant string) bool {
	fi, err := os.Stat(s.Path(hash, variant))
	return err == nil && fi.Mode().IsRegular()
}

// Size returns the committed file size, 0 if absent.
func (s *Store) Size(hash, variant string) (int64, error) {
	fi, err := os.Stat(s.Path(hash, variant))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Open returns a reader for a committed file (renderer path).
func (s *Store) Open(hash, variant string) (io.ReadCloser, error) {
	f, err := os.Open(s.Path(hash, variant))
	if err != nil {
		return nil, fmt.Errorf("cache: open %s %s: %w", hash, variant, err)
	}
	return f, nil
}

// Commit renames an already-verified .part file into place. It refuses
// to commit a path it does not own.
func (s *Store) Commit(hash, variant string) error {
	if !validVariant(variant) {
		return fmt.Errorf("cache: unknown variant %q", variant)
	}
	part, final := s.PartPath(hash, variant), s.Path(hash, variant)
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("cache: commit %s %s: %w", hash, variant, err)
	}
	// Best-effort permission tightening: photos are user data.
	_ = os.Chmod(final, 0o600)
	return nil
}

// RemovePart discards an in-progress download.
func (s *Store) RemovePart(hash, variant string) error {
	err := os.Remove(s.PartPath(hash, variant))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PartSize is the current size of a .part file (0 if absent) — used
// for Range-resume.
func (s *Store) PartSize(hash, variant string) (int64, error) {
	fi, err := os.Stat(s.PartPath(hash, variant))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Delete removes a committed file (GC path). Missing files are not an
// error.
func (s *Store) Delete(hash, variant string) error {
	err := os.Remove(s.Path(hash, variant))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// TotalSize is the sum of committed file sizes (excludes .part).
func (s *Store) TotalSize() (int64, error) {
	var total int64
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		total += fi.Size()
	}
	return total, nil
}

// SetQuota changes the cache quota at runtime (0 restores the
// free-space heuristic). Used by tests and by future settings UIs.
func (s *Store) SetQuota(quotaBytes int64) error {
	if quotaBytes < 0 {
		return fmt.Errorf("cache: negative quota %d", quotaBytes)
	}
	s.quotaBytes = quotaBytes
	return nil
}

// Free returns the free bytes on the volume holding the cache.
func (s *Store) Free() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &st); err != nil {
		return 0, fmt.Errorf("cache: statfs: %w", err)
	}
	return int64(st.Bavail) * st.Bsize, nil
}

// HasRoom reports whether a download of n additional bytes can proceed
// under the configured quota (or free-space floor). It is a pre-flight
// check; writers must still handle ENOSPC from a concurrent consumer.
func (s *Store) HasRoom(n int64) (bool, error) {
	if n < 0 {
		return false, fmt.Errorf("cache: negative size %d", n)
	}
	free, err := s.Free()
	if err != nil {
		return false, err
	}
	if s.quotaBytes > 0 {
		total, err := s.TotalSize()
		if err != nil {
			return false, err
		}
		return total+n <= s.quotaBytes, nil
	}
	return free-n >= s.minFreeBytes, nil
}

// GC removes committed files for which keep() returns false. It never
// touches .part files and never fails the whole pass for a single
// file. Returns the number of files removed.
func (s *Store) GC(keep func(hash, variant string) bool) (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		name := e.Name()
		dot := strings.LastIndex(name, ".")
		if dot <= 0 {
			continue
		}
		hash, variant := name[:dot], name[dot+1:]
		if !validVariant(variant) || keep(hash, variant) {
			continue
		}
		if os.Remove(filepath.Join(s.dir, name)) == nil {
			removed++
		}
	}
	return removed, nil
}
