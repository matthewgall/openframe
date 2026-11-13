//go:build windows

package updater

import "syscall"

// ownProcessGroupAttr returns a SysProcAttr that puts the child in its
// own process group (CREATE_NEW_PROCESS_GROUP), so stopping the frame
// stops its children too.
func ownProcessGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP
}

// termSignal is the polite stop signal for the frame process. Windows
// has no SIGTERM; the updater's stop path falls back to Kill.
var termSignal = syscall.SIGTERM
