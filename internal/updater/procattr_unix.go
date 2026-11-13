//go:build !windows

package updater

import "syscall"

// ownProcessGroupAttr returns a SysProcAttr that puts the child in its
// own process group, so stopping the frame stops its children too.
func ownProcessGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// termSignal is the polite stop signal for the frame process.
var termSignal = syscall.SIGTERM
