//go:build windows

package hub

import "syscall"

// sysProcAttrDetached returns SysProcAttr that creates a new process
// group on Windows. The Windows equivalent of Unix's setsid.
func sysProcAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
