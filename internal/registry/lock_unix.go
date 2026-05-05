//go:build !windows

package registry

import (
	"errors"
	"os"
	"syscall"
)

// tryLockExclusive attempts a non-blocking exclusive flock on f.
// Returns syscall.EWOULDBLOCK (or its alias EAGAIN) when contended,
// nil on success, or any other error from flock(2). Use the syscall.
// Flock primitive directly: it's available on all Unix variants bones
// supports (Linux, Darwin, BSDs) and avoids pulling x/sys.
func tryLockExclusive(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		// Some kernels (older Linux) surface EINTR around signal
		// handlers; retry transparently like the stdlib does.
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return err
	}
}

// unlock releases the flock on f. Best-effort: errors are ignored by
// the caller because closing the file also drops the lock.
func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
