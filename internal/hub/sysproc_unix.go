//go:build !windows

package hub

import "syscall"

// sysProcAttrDetached returns SysProcAttr that puts the spawned child
// into its own session and process group. This detaches it from the
// parent's controlling terminal: the child survives the parent's exit
// and no longer receives the parent's signals.
func sysProcAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
