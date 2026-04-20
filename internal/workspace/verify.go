package workspace

import (
	"os"
	"syscall"
)

// pidAlive returns true if a process with the given PID exists and accepts signal 0.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal(0) doesn't deliver a signal; it only checks existence/permissions.
	return proc.Signal(syscall.Signal(0)) == nil
}
