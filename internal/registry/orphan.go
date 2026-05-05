package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// reapGrace is how long Reap waits between SIGTERM and SIGKILL.
// Short by design — orphan hubs aren't holding state we want to flush;
// they're holding ports + unlinked fossil inodes we want released ASAP.
var reapGrace = 2 * time.Second

// IsOrphan reports whether e represents a process that is alive on
// this host but whose workspace is no longer reachable. Three signals
// qualify a workspace as gone:
//
//  1. e.Cwd does not exist on disk (ENOENT)
//  2. e.Cwd exists but its workspace marker (.bones/agent.id) does not
//  3. e.Cwd resolves into the user's Trash (~/.Trash on macOS, the
//     XDG-Trash equivalent on Linux)
//
// The PID-alive check is the same one IsAlive uses; an entry whose
// PID is dead is not an orphan (it's a stale entry awaiting prune)
// and will be reported by IsAlive returning false.
func IsOrphan(e Entry) bool {
	if !pidAlive(e.HubPID) {
		return false
	}
	if _, err := os.Stat(e.Cwd); os.IsNotExist(err) {
		return true
	}
	marker := filepath.Join(e.Cwd, ".bones", "agent.id")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		return true
	}
	if isTrashed(e.Cwd) {
		return true
	}
	return false
}

// isTrashed reports whether path is inside the user's Trash. macOS
// uses ~/.Trash; Linux uses ~/.local/share/Trash by default. The
// check is path-prefix only — if a process legitimately runs from
// inside Trash (rare), it is treated as orphan; the reaper requires
// confirmation by default so this edge case is recoverable.
func isTrashed(path string) bool {
	home := os.Getenv("HOME")
	if home == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	candidates := []string{filepath.Join(home, ".Trash")}
	if runtime.GOOS == "linux" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "share", "Trash"))
	}
	for _, c := range candidates {
		if strings.HasPrefix(abs, c+string(filepath.Separator)) || abs == c {
			return true
		}
	}
	return false
}

// Orphans returns all registry entries whose process is alive but
// whose workspace is gone. Read-only; the caller decides what to do.
func Orphans() ([]Entry, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range all {
		if IsOrphan(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

// Reap terminates the process for e and removes its registry entry.
// SIGTERM first; if the PID is still alive after reapGrace, SIGKILL.
// Returns nil on success (process gone, entry removed) or an error
// describing which step failed. The entry is removed even after
// SIGKILL — leaving it would create a permanent registry record for
// a process that, by definition, isn't going to come back.
func Reap(e Entry) error {
	if !pidAlive(e.HubPID) {
		return RemoveByPID(e.Cwd, e.HubPID)
	}
	proc, err := os.FindProcess(e.HubPID)
	if err != nil {
		return fmt.Errorf("registry.Reap: find pid %d: %w", e.HubPID, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("registry.Reap: SIGTERM pid %d: %w", e.HubPID, err)
	}
	deadline := time.Now().Add(reapGrace)
	for time.Now().Before(deadline) {
		if !pidAlive(e.HubPID) {
			return RemoveByPID(e.Cwd, e.HubPID)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("registry.Reap: SIGKILL pid %d: %w", e.HubPID, err)
	}
	// Best-effort wait for SIGKILL to take effect; remove entry regardless.
	for i := 0; i < 20; i++ {
		if !pidAlive(e.HubPID) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return RemoveByPID(e.Cwd, e.HubPID)
}
