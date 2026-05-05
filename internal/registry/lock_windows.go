//go:build windows

package registry

import (
	"fmt"
	"os"
)

// tryLockExclusive on Windows is a best-effort pid-file probe; flock
// is not available there. AcquireWorkspaceLock reads the recorded
// holder pid before opening the lock file, so the contention check
// happens before this function. Here we only succeed when the
// underlying file open already worked. The pid-aliveness decision
// lives in AcquireWorkspaceLock.
//
// Trade-off: two genuine concurrent starts that both race past the
// pid-file write can both succeed. The downstream port-collision
// check at bind time (#138 item 1) catches them. macOS + Linux use
// flock and don't have this race window; Windows relies on the
// per-pid registry duplicate-hub WARN to surface any leak.
func tryLockExclusive(f *os.File) error {
	if f == nil {
		return fmt.Errorf("workspace lock: nil file handle")
	}
	return nil
}

// unlock is a no-op on Windows; closing the file is the only release
// step the caller needs.
func unlock(f *os.File) error {
	return nil
}
