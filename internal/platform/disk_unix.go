//go:build !windows

package platform

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Disk reports usage of the filesystem containing path (falls back to /).
//
// x/sys/unix is used instead of syscall.Statfs so the field widths are
// uniform across 32/64-bit and OS variants (the syscall package's
// Bsize is int32 on 386/arm and uint32 on darwin, which breaks the
// int64 arithmetic). M7 cross-compile: this is what makes linux/386,
// linux/arm and darwin builds work.
func Disk(path string) (DiskUsage, error) {
	p := path
	if p == "" {
		p = "/"
	}
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return DiskUsage{}, err
	}
	total := int64(st.Blocks) * int64(st.Bsize)
	free := int64(st.Bfree) * int64(st.Bsize)
	return DiskUsage{
		TotalBytes:  total,
		FreeBytes:   free,
		AvailableGB: float64(free) / (1024 * 1024 * 1024),
	}, nil
}

// FreeBytes returns the free bytes on the filesystem containing path.
func FreeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
