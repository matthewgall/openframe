// Package platform provides host introspection used by `openframe
// doctor` and `openframe status`.
//
// It is deliberately read-only and side-effect free: doctor must be safe
// to run on a production frame without changing anything.
package platform

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MemoryInfo is a summary of system memory.
type MemoryInfo struct {
	TotalBytes int64 `json:"total_bytes"`
	Available  int64 `json:"available_bytes"`
}

// DiskUsage describes a filesystem.
type DiskUsage struct {
	TotalBytes  int64   `json:"total_bytes"`
	FreeBytes   int64   `json:"free_bytes"`
	AvailableGB float64 `json:"available_gb"`
}

// DisplayInfo describes what display capability the host exposes.
type DisplayInfo struct {
	Present  bool     `json:"present"`
	Backends []string `json:"backends"` // e.g. ["drm:/dev/dri/card0"] or ["x11:1.0"]
	Notes    string   `json:"notes,omitempty"`
}

// TouchInfo describes detected touch input devices.
type TouchInfo struct {
	Present bool     `json:"present"`
	Devices []string `json:"devices"` // e.g. ["event0:USB HID"]
}

// Memory reads /proc/meminfo (Linux). Returns zero values with TotalBytes
// = 0 on non-Linux hosts; doctor reports that as "unknown".
func Memory() MemoryInfo {
	var m MemoryInfo
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		bytes := val * 1024
		switch fields[0] {
		case "MemTotal:":
			m.TotalBytes = bytes
		case "MemAvailable:":
			m.Available = bytes
		}
	}
	return m
}

// Disk reports usage of the filesystem containing path (falls back to /).
func Disk(path string) (DiskUsage, error) {
	var st syscall.Statfs_t
	p := path
	if p == "" {
		p = "/"
	}
	if err := syscall.Statfs(p, &st); err != nil {
		return DiskUsage{}, err
	}
	total := int64(st.Blocks) * st.Bsize
	free := int64(st.Bfree) * st.Bsize
	return DiskUsage{
		TotalBytes:  total,
		FreeBytes:   free,
		AvailableGB: float64(free) / (1024 * 1024 * 1024),
	}, nil
}

// DetectDisplay inspects common Linux display backends.
//
// M1 does not drive any display; this only reports what a milestone-4
// renderer could use. Order of preference for the DRM/KMS decision:
// /dev/dri/card* first (headless kiosk), then X11/Wayland for dev boxes.
func DetectDisplay() DisplayInfo {
	var info DisplayInfo

	if runtime.GOOS != "linux" {
		info.Notes = "display detection is only implemented for linux"
		return info
	}

	entries, err := os.ReadDir("/dev/dri")
	if err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "card") {
				dev := "/dev/dri/" + e.Name()
				// card0 is usually video; card1+ on Pi are often the
				// HDMI output. We list all and let doctor annotate.
				info.Present = true
				info.Backends = append(info.Backends, "drm:"+dev)
			}
		}
	}

	if d := os.Getenv("DISPLAY"); d != "" {
		info.Present = true
		info.Backends = append(info.Backends, "x11:"+d)
	}
	if wl := os.Getenv("WAYLAND_DISPLAY"); wl != "" {
		info.Present = true
		info.Backends = append(info.Backends, "wayland:"+wl)
	}

	if !info.Present {
		info.Notes = "no display backend found (headless frame runs fine with the dev renderer)"
	}
	return info
}

// DetectTouch lists /dev/input/event* devices whose names suggest touch
// input. Names are read from the input subsystem; devices without a
// readable name are listed generically.
func DetectTouch() TouchInfo {
	var info TouchInfo
	entries, err := os.ReadDir("/dev/input")
	if err != nil {
		return info
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "event") {
			continue
		}
		dev := filepath.Join("/dev/input", e.Name())
		name := readInputName(dev)
		if name == "" {
			// Unreadable name (permissions): do not guess that the
			// device is a touchscreen.
			continue
		}
		upper := strings.ToUpper(name)
		if strings.Contains(upper, "TOUCH") || strings.Contains(upper, "EGALAX") ||
			strings.Contains(upper, "MULTITOUCH") {
			info.Present = true
			info.Devices = append(info.Devices, e.Name()+": "+name)
		}
	}
	return info
}

// readInputName returns the human-readable name of an evdev device, or ""
// if it cannot be read (permissions). It uses the EVIOCGNAME ioctl.
func readInputName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	const eviocgname = 0x80184600 // EVIOCGNAME(32)
	buf := make([]byte, 32)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), eviocgname, uintptr(uintptr(len(buf))))
	_ = errno
	name := strings.TrimRight(string(buf), "\x00")
	return name
}

// Reachable probes a URL over HTTPS/HTTP with a bounded timeout.
func Reachable(ctx context.Context, url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// HostInfo is a snapshot for doctor output.
type HostInfo struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Go       string `json:"go"`
	Hostname string `json:"hostname"`
}

func Host() HostInfo {
	h, _ := os.Hostname()
	return HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(), Hostname: h}
}
