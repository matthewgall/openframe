//go:build !windows

package touch

import (
	"fmt"
	"syscall"
)

// createFifo creates an empty named pipe (the dev input transport).
// Unix-only: Windows has no FIFOs, so the dev input feature is
// unavailable there (the config field is simply inert).
func createFifo(path string) error {
	if err := syscall.Mknod(path, 0o622|syscall.S_IFIFO, 0); err != nil {
		return fmt.Errorf("dev input: creating fifo: %w", err)
	}
	return nil
}
