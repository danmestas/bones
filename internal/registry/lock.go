package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LockFileName is the basename of the workspace-scoped lock file
// `bones hub start` acquires before any port bind, URL-file write, or
// hub bootstrap. Lives at <root>/.bones/hub.lock alongside the URL
// files and pid files (per the issue brief: "workspace-scoped lock
// file path lives under the workspace's bones-state directory"). The
// sibling hub.lock.pid records the holder for human-readable error
// messages.
const (
	LockFileName    = "hub.lock"
	LockPidFileName = "hub.lock.pid"
)

// ErrLockHeld is returned by AcquireWorkspaceLock when another live
// process holds the workspace lock. The PID is the holder (parsed
// from hub.lock.pid); callers surface it in CLI output so the
// operator can act.
type ErrLockHeld struct {
	PID  int
	Path string
}

func (e *ErrLockHeld) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf(
			"workspace hub lock %s held by pid %d", e.Path, e.PID)
	}
	return fmt.Sprintf("workspace hub lock %s held by another process", e.Path)
}

// LockPath returns the absolute path of the workspace lock file for
// the workspace at root.
func LockPath(root string) string {
	return filepath.Join(root, ".bones", LockFileName)
}

// LockPidPath returns the absolute path of the sibling pid file
// recording the lock holder.
func LockPidPath(root string) string {
	return filepath.Join(root, ".bones", LockPidFileName)
}

// AcquireWorkspaceLock takes an exclusive non-blocking advisory lock
// on <root>/.bones/hub.lock. Used by `bones hub start` to refuse a
// second concurrent start against the same workspace before any side
// effects (port bind, URL-file write, fork-exec).
//
// Returns a release func that drops the lock + removes the sibling
// pid file. If the lock is held by a live process, returns
// *ErrLockHeld. If the sibling pid file names a dead process, the
// stale pid is overwritten and the lock is reclaimed (the kill -9
// recovery path from the issue's acceptance criteria).
//
// Cross-platform: on Unix this is syscall.Flock(LOCK_EX|LOCK_NB); on
// Windows it falls back to a best-effort pid-file probe (see
// lock_windows.go) since flock is not available there.
func AcquireWorkspaceLock(root string) (func(), error) {
	bones := filepath.Join(root, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		return nil, fmt.Errorf("workspace lock: mkdir: %w", err)
	}
	path := LockPath(root)
	pidPath := LockPidPath(root)

	// Pid-file contention check (cross-platform). flock is the source
	// of truth on Unix; Windows does not have flock(2), so we treat
	// "holder pid in lock.pid is alive" as the contention signal there.
	// On Unix this is just an early human-readable error: flock will
	// also fail and we'd still surface the holder pid below.
	if holder, ok := readLockHolderPID(pidPath); ok && holder != os.Getpid() && pidAlive(holder) {
		return nil, &ErrLockHeld{PID: holder, Path: path}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("workspace lock: open: %w", err)
	}
	if err := tryLockExclusive(f); err != nil {
		holder, _ := readLockHolderPID(pidPath)
		_ = f.Close()
		// Stale-holder reclamation: if the recorded holder is dead,
		// delete the pid file and retry once. The flock itself is
		// already released (the dead process exited), so the retry
		// succeeds.
		if holder > 0 && !pidAlive(holder) {
			_ = os.Remove(pidPath)
			f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				return nil, fmt.Errorf("workspace lock: reopen: %w", err)
			}
			if err := tryLockExclusive(f); err != nil {
				_ = f.Close()
				return nil, &ErrLockHeld{PID: holder, Path: path}
			}
		} else {
			return nil, &ErrLockHeld{PID: holder, Path: path}
		}
	}
	if err := writeLockHolderPID(pidPath, os.Getpid()); err != nil {
		_ = unlock(f)
		_ = f.Close()
		return nil, fmt.Errorf("workspace lock: pid file: %w", err)
	}
	release := func() {
		_ = os.Remove(pidPath)
		_ = unlock(f)
		_ = f.Close()
	}
	return release, nil
}

// readLockHolderPID parses the pid recorded in hub.lock.pid. Returns
// (0, false) on any read or parse error.
func readLockHolderPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return pid, true
}

// writeLockHolderPID atomically replaces the pid file with our pid.
// Tmp+rename keeps a torn read off the table for an external observer
// (e.g. `bones doctor` reading the file between truncate and write).
func writeLockHolderPID(path string, pid int) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write([]byte(strconv.Itoa(pid) + "\n")); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
