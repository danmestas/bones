package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WorkspaceID returns a deterministic 16-hex-char identifier for an absolute cwd.
// Used as the registry filename prefix: ~/.bones/workspaces/<id>.json.
func WorkspaceID(cwd string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return hex.EncodeToString(sum[:])[:16]
}

// Entry is one workspace's registry record. Each `bones hub start`
// writes its own entry at ~/.bones/workspaces/<WorkspaceID>.json —
// one file per workspace (the supervisor model). Older bones binaries
// used the per-PID layout `<id>-<pid>.json` (pre-#250); the read path
// migrates legacy files into the canonical name silently on first read.
type Entry struct {
	Cwd       string    `json:"cwd"`
	Name      string    `json:"name"`
	HubURL    string    `json:"hub_url"`
	NATSURL   string    `json:"nats_url"`
	HubPID    int       `json:"hub_pid"`
	StartedAt time.Time `json:"started_at"`
}

// RegistryDir returns the directory that holds workspace entry files.
func RegistryDir() string {
	return filepath.Join(os.Getenv("HOME"), ".bones", "workspaces")
}

// EntryPath returns the absolute path of the JSON file for a given
// workspace cwd. The PID is no longer encoded in the filename
// (issue #250); the workspace registry is single-file-per-workspace
// and last-writer-wins. Per-host orphan visibility is now provided
// by `bones status --all` (issue #264) which scans the host process
// table — superseding the duplicate-detection role the PID-suffix
// originally played in #208.
func EntryPath(cwd string) string {
	return filepath.Join(RegistryDir(), WorkspaceID(cwd)+".json")
}

// Write persists e to its file atomically (tmp+rename). Creates the registry
// directory if missing. Filename is keyed by cwd alone — see EntryPath.
// Last-write-wins on the file is the intended behavior in the
// supervisor model (one hub per workspace).
func Write(e Entry) error {
	if err := os.MkdirAll(RegistryDir(), 0o755); err != nil {
		return fmt.Errorf("registry mkdir: %w", err)
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("registry marshal: %w", err)
	}

	dst := EntryPath(e.Cwd)
	tmp, err := os.CreateTemp(RegistryDir(), filepath.Base(dst)+".tmp.*")
	if err != nil {
		return fmt.Errorf("registry tmp: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("registry write: %w", err), closeErr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("registry sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("registry close: %w", err)
	}
	return os.Rename(tmp.Name(), dst)
}

// ErrNotFound is returned by Read when no entry exists for the given cwd.
var ErrNotFound = errors.New("registry: entry not found")

// Read loads the workspace's registry entry. Single-file layout (#250):
// the canonical path is <id>.json. Legacy per-PID files (`<id>-<pid>.json`)
// are migrated on read into the canonical name and then deleted —
// silent, best-effort. If multiple legacy entries exist the alive-PID
// one wins (or the most recent if all are dead). If the canonical file
// AND legacy files both exist (a half-migrated workspace) the canonical
// file is preferred and the legacy files are removed.
//
// Self-prunes the workspace's entry on read (#229): if the chosen entry's
// HubPID is dead OR its Cwd no longer exists, the file is removed before
// returning ErrNotFound.
func Read(cwd string) (Entry, error) {
	matches, err := matchingPaths(cwd)
	if err != nil {
		return Entry{}, err
	}
	if len(matches) == 0 {
		return Entry{}, ErrNotFound
	}

	canonical := EntryPath(cwd)
	var best Entry
	var bestPath string
	var bestSet bool
	for _, p := range matches {
		e, err := readEntryAtPath(p)
		if err != nil {
			continue
		}
		if isStaleEntry(e) {
			_ = os.Remove(p)
			continue
		}
		switch {
		case !bestSet,
			pidAlive(e.HubPID) && !pidAlive(best.HubPID),
			pidAlive(e.HubPID) == pidAlive(best.HubPID) && e.StartedAt.After(best.StartedAt):
			best = e
			bestPath = p
			bestSet = true
		}
	}
	if !bestSet {
		return Entry{}, ErrNotFound
	}

	// One-shot migration: if the chosen entry is in a legacy path or
	// non-canonical files still exist, collapse to <id>.json. Best-effort
	// — migration errors do not fail the read.
	migrateToCanonical(matches, bestPath, canonical)

	return best, nil
}

// migrateToCanonical collapses any legacy <id>-<pid>.json files plus
// half-migrated state into a single <id>.json. matches is the full set
// of files matchingPaths returned for this cwd; bestPath is the survivor
// chosen by Read; canonical is EntryPath(cwd).
//
// Strategy:
//
//   - If bestPath != canonical, atomically rename bestPath to canonical.
//   - Delete every other path in matches (whether legacy or stray).
//
// All errors are swallowed — the caller has already extracted the
// in-memory Entry, so the on-disk shape is best-effort cleanup.
func migrateToCanonical(matches []string, bestPath, canonical string) {
	if bestPath != canonical {
		// Move chosen file to canonical name. Rename across the same dir
		// is atomic on POSIX. If the canonical file already exists (the
		// half-migrated case) Rename overwrites it on Linux/macOS.
		_ = os.Rename(bestPath, canonical)
	}
	for _, p := range matches {
		if p == canonical || p == bestPath {
			continue
		}
		_ = os.Remove(p)
	}
}

// readEntryAtPath is the path-keyed sibling of Read. Read is cwd-keyed
// and walks every matching <id>*.json. Lives here as a shared helper
// for List/ListInfo/Read.
func readEntryAtPath(path string) (Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// List returns all registry entries, skipping corrupt files.
//
// Self-prunes stale entries on read (#229): any entry whose HubPID is
// not alive on this host OR whose Cwd no longer exists is deleted from
// disk before the surviving set is returned. ADR 0043 promises the
// registry "prunes on read"; pre-#229 only the in-memory filter
// honored that — the on-disk files accumulated indefinitely.
func List() ([]Entry, error) {
	_, entries, err := pruneStale()
	if err != nil {
		return nil, fmt.Errorf("registry glob: %w", err)
	}
	return entries, nil
}

// Remove deletes the registry entry for the given workspace cwd —
// the canonical <id>.json plus any legacy <id>-<pid>.json files left
// over from pre-#250 layouts. Idempotent. Used by `bones down` and
// stale-entry pruners that operate at workspace granularity.
func Remove(cwd string) error {
	matches, err := matchingPaths(cwd)
	if err != nil {
		return fmt.Errorf("registry remove: %w", err)
	}
	var firstErr error
	for _, p := range matches {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return fmt.Errorf("registry remove: %w", firstErr)
	}
	return nil
}

// matchingPaths returns every file in RegistryDir that belongs to the
// given workspace cwd: the canonical <id>.json AND every legacy per-pid
// <id>-<pid>.json (kept for migration on read, see #250). Result order
// is unspecified.
func matchingPaths(cwd string) ([]string, error) {
	id := WorkspaceID(cwd)
	dir := RegistryDir()
	// Glob legacy per-pid files <id>-*.json so half-migrated workspaces
	// still surface their entry on read.
	matches, err := filepath.Glob(filepath.Join(dir, id+"-*.json"))
	if err != nil {
		return nil, fmt.Errorf("registry glob: %w", err)
	}
	// Filter out atomic-write tmp files surfaced by the glob.
	out := matches[:0]
	for _, p := range matches {
		if strings.Contains(filepath.Base(p), ".tmp.") {
			continue
		}
		out = append(out, p)
	}
	canonical := EntryPath(cwd)
	if _, err := os.Stat(canonical); err == nil {
		out = append(out, canonical)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("registry stat canonical: %w", err)
	}
	return out, nil
}
