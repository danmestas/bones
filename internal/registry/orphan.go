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
// PID is dead is not an orphan (it's a stale entry that the read-
// time prune will delete; see prune.go).
//
// Since #229's read-time self-prune, signal (1) and the dead-PID
// case are removed at the registry layer before IsOrphan is reached.
// IsOrphan still tests them defensively in case a new caller routes
// around List/Orphans, but in practice this function returns true
// only for the marker-missing or trashed-cwd signals.
func IsOrphan(e Entry) bool {
	if !pidAlive(e.HubPID) {
		return false
	}
	if _, err := os.Stat(e.Cwd); os.IsNotExist(err) {
		return true
	}
	marker := filepath.Join(bonesDir(e.Cwd), "agent.id")
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
//
// Deprecated: prefer AllOrphanHubs, which also surfaces process-only
// orphans (live `bones hub start` PIDs with no registry entry at all).
// Retained for the existing test surface; new callers should not use
// it.
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

// OrphanSource discriminates how an OrphanHub was discovered. Process-
// only orphans (no registry entry at all) used to be invisible to
// `bones hub reap`, `bones doctor`, and `bones down` because those
// callers all used Orphans() which only filters List(). AllOrphanHubs
// unions both kinds; the Source field tells the caller which fields
// of OrphanHub are populated.
type OrphanSource int

const (
	// SourceRegistry: the orphan was discovered via the registry
	// (entry exists, IsOrphan returned true). Entry is fully populated.
	SourceRegistry OrphanSource = iota
	// SourceProcess: the orphan is a live `bones hub start` process
	// with no matching registry entry. Process is populated; Entry is
	// the zero value.
	SourceProcess
)

// OrphanHub is the unified shape returned by AllOrphanHubs. The Source
// discriminant tells callers which struct fields are meaningful:
//
//   - Source==SourceRegistry: Entry is populated; PID/Cwd are mirrored
//     from Entry.HubPID/Entry.Cwd for renderer convenience.
//   - Source==SourceProcess: Process is populated; Entry is zero;
//     PID/Cwd are mirrored from Process.PID/Process.Cwd. The renderer
//     uses Process.ETime in lieu of Entry.StartedAt.
//
// Reason carries a short human-readable string describing why the
// orphan was flagged; used by `bones status --all`'s tabular output.
type OrphanHub struct {
	Source  OrphanSource
	Entry   Entry
	Process HubProcess
	PID     int
	Cwd     string
	Reason  string
}

// liveHubProcessesFn is the live-process scanner AllOrphanHubs calls.
// Production sets it to LiveHubProcesses; tests can swap it for canned
// HubProcess slices to exercise the process-source orphan path without
// needing to spawn a real `bones hub start` subprocess.
var liveHubProcessesFn = LiveHubProcesses

// AllOrphanHubs returns the union of registry-side orphans (entries
// where IsOrphan is true) and process-only orphans (live `bones hub
// start` PIDs with no matching registry entry).
//
// Discovery proceeds in two passes:
//
//  1. List() + IsOrphan filter — produces SourceRegistry rows.
//  2. LiveHubProcesses() minus the live PIDs from (1)+remaining List
//     entries — produces SourceProcess rows for processes the
//     registry doesn't account for.
//
// Cwd comparison uses ResolveCwd so a workspace path like /tmp/foo
// compares equal to the lsof-resolved /private/tmp/foo on macOS
// (#353).
//
// Best-effort: a LiveHubProcesses failure (e.g. no `ps` on PATH) is
// not fatal — only registry-source orphans are returned in that case,
// alongside the underlying error.
func AllOrphanHubs() ([]OrphanHub, error) {
	entries, err := List()
	if err != nil {
		return nil, fmt.Errorf("registry.AllOrphanHubs: list: %w", err)
	}

	out := make([]OrphanHub, 0)
	registryByCwd := make(map[string]Entry, len(entries))
	registryByPID := make(map[int]struct{}, len(entries))
	for _, e := range entries {
		registryByCwd[ResolveCwd(e.Cwd)] = e
		if e.HubPID > 0 {
			registryByPID[e.HubPID] = struct{}{}
		}
		if IsOrphan(e) {
			out = append(out, OrphanHub{
				Source: SourceRegistry,
				Entry:  e,
				PID:    e.HubPID,
				Cwd:    e.Cwd,
				Reason: registryOrphanReason(e),
			})
		}
	}

	procs, procErr := liveHubProcessesFn()
	if procErr != nil {
		// Process-table read failed — return whatever registry-side
		// orphans we found alongside the error so callers can still
		// render Section 1 of status --all.
		return out, fmt.Errorf("registry.AllOrphanHubs: live processes: %w", procErr)
	}

	for _, p := range procs {
		// If a registry entry already covers this PID, the entry is
		// the canonical orphan record (Section 2 of status --all
		// surfaces process-only orphans; Section 3 surfaces paused
		// registry entries; here we only care about the cross set).
		if _, ok := registryByPID[p.PID]; ok {
			continue
		}
		// If the process's cwd matches a known registry entry's cwd
		// (modulo symlink resolution), it's an active or paused
		// workspace, not a process-only orphan.
		if p.Cwd != "" {
			if _, ok := registryByCwd[ResolveCwd(p.Cwd)]; ok {
				continue
			}
		}
		out = append(out, OrphanHub{
			Source:  SourceProcess,
			Process: p,
			PID:     p.PID,
			Cwd:     p.Cwd,
			Reason:  processOrphanReason(p),
		})
	}

	return out, nil
}

// registryOrphanReason returns the human-readable reason for a
// registry-side orphan (mirrors the doctor/down phrasing).
func registryOrphanReason(e Entry) string {
	if _, err := os.Stat(e.Cwd); os.IsNotExist(err) {
		return "cwd no longer exists"
	}
	marker := filepath.Join(bonesDir(e.Cwd), "agent.id")
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		return "workspace marker missing (.bones/agent.id)"
	}
	if isTrashed(e.Cwd) {
		return "cwd is in Trash"
	}
	return "registry entry orphaned"
}

// processOrphanReason returns the reason string for a process-only
// orphan (no matching registry entry).
func processOrphanReason(p HubProcess) string {
	if p.Cwd == "" {
		return "cwd unknown (process introspection failed)"
	}
	if _, err := os.Stat(p.Cwd); os.IsNotExist(err) {
		return "cwd no longer exists"
	}
	return "cwd missing from registry"
}

// ResolveCwd returns the symlink-resolved absolute form of p, falling
// back to filepath.Clean(p) when EvalSymlinks fails (path doesn't
// exist, permission denied, broken symlink chain). Used to normalize
// both registry-side cwds (as stored, e.g. "/tmp/foo") and process-
// side cwds (as lsof returns them, e.g. "/private/tmp/foo") to the
// same canonical form before comparison.
//
// Without this normalization a workspace at /tmp/foo on macOS (where
// /tmp is a symlink to /private/tmp) shows in `bones status --all`
// twice (#353); the registry entry keyed by /tmp/foo (which preserves
// /tmp) misses the lookup keyed by the lsof-resolved /private/tmp
// path.
//
// EvalSymlinks does NOT make a filesystem call when p has no symlink
// components, so the fallback path on Linux (where /tmp is a real
// directory) costs roughly the same as filepath.Clean.
func ResolveCwd(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// Reap terminates the process for e and removes its registry entry.
// Thin wrapper over ReapEntry preserved for backwards compatibility
// with the existing test surface; new callers should select between
// ReapEntry (registry-tracked) and ReapPID (process-only) explicitly.
func Reap(e Entry) error {
	return ReapEntry(e)
}

// ReapEntry terminates the process for e and removes its registry
// entry. SIGTERM first; if the PID is still alive after reapGrace,
// SIGKILL. Returns nil on success (process gone, entry removed) or
// an error describing which step failed. The entry is removed even
// after SIGKILL — leaving it would create a permanent registry
// record for a process that, by definition, isn't going to come back.
func ReapEntry(e Entry) error {
	if !pidAlive(e.HubPID) {
		return Remove(e.Cwd)
	}
	if err := killPID(e.HubPID); err != nil {
		return err
	}
	return Remove(e.Cwd)
}

// ReapPID terminates pid (SIGTERM-then-SIGKILL like ReapEntry) but
// does not touch the registry. Used for process-source orphans
// surfaced by AllOrphanHubs that have no registry entry to remove.
// A no-op if pid is already dead.
func ReapPID(pid int) error {
	if !pidAlive(pid) {
		return nil
	}
	return killPID(pid)
}

// killPID is the shared signal+grace-period helper. SIGTERM, wait
// reapGrace polling pidAlive at 50ms; if still alive, SIGKILL and
// best-effort wait at 25ms x 20.
func killPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("registry.Reap: find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("registry.Reap: SIGTERM pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(reapGrace)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("registry.Reap: SIGKILL pid %d: %w", pid, err)
	}
	for range 20 {
		if !pidAlive(pid) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil
}
