//go:build windows

package touch

import "errors"

// createFifo on Windows: FIFOs do not exist there, so the dev input
// transport (and therefore the DevInput config field) is unavailable.
// The frame itself works; only this development seam does not.
var errDevInputUnsupported = errors.New("dev input: FIFOs are not available on Windows (use a unix host for headless UI testing)")

func createFifo(path string) error { return errDevInputUnsupported }
