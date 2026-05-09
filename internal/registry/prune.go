package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// pruneStale walks every <id>-<pid>.json (and legacy <id>.json) in the
// registry directory, deletes any entry whose process is not alive on
// this host OR whose recorded Cwd no longer exists, and returns the
// surviving entries paired with their on-disk paths.
//
// This is the read-time self-prune from issue #229: pre-fix the
// registry only filtered stale entries in memory and they accumulated
// indefinitely (PR #199-era reaper required manual `bones hub reap`).
// Per ADR 0043 the registry should self-clean — pidAlive == false and
// missing-cwd are both "this entry is meaningless" signals — so a
// single read pass is the right place to act.
//
// The doctor's "orphan" report (live PID + workspace marker missing
// or trashed) is preserved as actionable: those entries still have an
// existing Cwd directory and a live PID, so they fall through the
// prune and surface via Orphans(). Only the silent-crud cases (dead
// PID, vanished cwd) get deleted.
//
// Errors deleting individual files are swallowed — the goal is
// best-effort cleanup, not a transactional sweep. A file that survives
// (permissions glitch, race with another reader) just shows up on the
// next call. Returns parallel slices: paths[i] is the on-disk path of
// entries[i]. Callers that don't need paths can ignore the first
// return value.
func pruneStale() ([]string, []Entry, error) {
	matches, err := filepath.Glob(filepath.Join(RegistryDir(), "*.json"))
	if err != nil {
		return nil, nil, err
	}
	paths := make([]string, 0, len(matches))
	entries := make([]Entry, 0, len(matches))
	for _, path := range matches {
		// Skip atomic-write tmp files surfaced by the glob.
		if strings.Contains(filepath.Base(path), ".tmp.") {
			continue
		}
		e, err := readEntryAtPath(path)
		if err != nil {
			// Corrupt files are not stale-by-pid; leave them so
			// existing List behavior (skip-on-error) is preserved.
			// A separate corruption-prune verb is out of scope for #229.
			continue
		}
		if isStaleEntry(e) {
			_ = os.Remove(path)
			continue
		}
		paths = append(paths, path)
		entries = append(entries, e)
	}
	return paths, entries, nil
}

// isStaleEntry reports whether e is registry crud that the read-path
// scan should delete. Three signals qualify:
//
//  1. e.Cwd does not exist on disk (ENOENT). The workspace was rm
//     -rf'd; even if the PID happens to be alive (recycled to an
//     unrelated process, or a true orphan), the entry has no
//     reachable workspace to act against.
//  2. e.HubPID > 0 but is not alive on this host (dead PID, never-
//     existed PID, or recycled-to-other-process PID we don't have
//     permission to signal). The original hub is gone; nothing this
//     entry points to still exists.
//
// PID=0 entries are NOT stale: they're "registered but idle"
// workspaces written by `bones up` (#305). They carry a valid cwd
// and no claimed hub PID; they exist so a freshly-up'd workspace is
// visible to `bones status --all` between `bones up` and the first
// verb that triggers a hub serve. `bones down` deletes these via
// Remove; a hub start overwrites them with a PID-bearing entry.
//
// Live PID + cwd-exists-but-marker-missing-or-trashed entries are
// left alone — those are doctor-actionable orphans (ADR 0043) that
// the operator should resolve via `bones hub reap`.
func isStaleEntry(e Entry) bool {
	if _, err := os.Stat(e.Cwd); errors.Is(err, os.ErrNotExist) {
		return true
	}
	if e.HubPID == 0 {
		// Registered-but-idle (#305): no claimed hub, valid cwd. Keep.
		return false
	}
	if !pidAlive(e.HubPID) {
		return true
	}
	return false
}
