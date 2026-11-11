// Package version carries build-time version information.
//
// Values are injected at build time via -ldflags, e.g.:
//
//	-X github.com/openframe/openframe/internal/version.Version=1.2.3
//	-X github.com/openframe/openframe/internal/version.Commit=abc1234
//	-X github.com/openframe/openframe/internal/version.Date=2026-01-01T00:00:00Z
package version

import (
	"fmt"
	"runtime"
	"strings"
)

// Build metadata. Overridden at link time by the Makefile / release pipeline.
var (
	Version = "0.1.0-dev"
	Commit  = "none"
	Date    = "unknown"
)

// Info is a serialisable snapshot of the build metadata.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Current returns the build metadata for the running binary.
func Current() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
}

// String renders a human-readable multi-line version block.
func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "openframe %s\n", i.Version)
	fmt.Fprintf(&b, "  commit: %s\n", i.Commit)
	fmt.Fprintf(&b, "  built:  %s\n", i.Date)
	fmt.Fprintf(&b, "  go:     %s\n", i.Go)
	fmt.Fprintf(&b, "  os:     %s/%s\n", i.OS, i.Arch)
	return b.String()
}
