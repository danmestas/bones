package cli

import (
	"os"
	"syscall"
	"time"
)

// pidAlive reports whether pid is a live process this user can signal.
// Signal 0 is the standard "is the kernel still tracking this pid"
// probe — it performs the lookup without delivering anything. Any
// error (no such process, permission denied, malformed pid) returns
// false. Mirrors internal/workspace/verify.go's helper, duplicated
// here so the cli package does not import internal/workspace's
// unexported function. ADR 0028 §"Process lifecycle and crash
// recovery" calls out this exact probe.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// timeNow is the time source for swarm verbs. Plain wrapper today;
// pulled into a function so future test-time injection (e.g. a fixed
// clock in unit tests) is a one-line change rather than a refactor.
func timeNow() time.Time {
	return time.Now().UTC()
}
