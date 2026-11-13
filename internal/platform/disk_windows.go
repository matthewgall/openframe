//go:build windows

package platform

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// diskFree calls GetDiskFreeSpaceEx and returns (total, totalFree);
// the "available to caller" figure is unused (we want total free).
func diskFree(path string) (total, totalFree uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeAvail uint64
	err = windows.GetDiskFreeSpaceEx(p, &freeAvail, &total, &totalFree)
	return total, totalFree, err
}

// Disk reports usage of the volume containing path (falls back to C:\).
// Windows has no statfs; GetDiskFreeSpaceEx is the equivalent. M7
// cross-compile: this is what makes the windows builds work.
func Disk(path string) (DiskUsage, error) {
	if path == "" {
		path = `C:\`
	}
	total, totalFree, err := diskFree(path)
	if err != nil {
		return DiskUsage{}, err
	}
	return DiskUsage{
		TotalBytes:  int64(total),
		FreeBytes:   int64(totalFree),
		AvailableGB: float64(int64(totalFree)) / (1024 * 1024 * 1024),
	}, nil
}

// FreeBytes returns the free bytes on the volume containing path.
func FreeBytes(path string) (int64, error) {
	_, totalFree, err := diskFree(path)
	if err != nil {
		return 0, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", path, err)
	}
	return int64(totalFree), nil
}
