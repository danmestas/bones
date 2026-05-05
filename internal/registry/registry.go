package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// WorkspaceID returns a deterministic 16-hex-char identifier for an absolute cwd.
// Used as the registry filename prefix: ~/.bones/workspaces/<id>-<pid>.json.
func WorkspaceID(cwd string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return hex.EncodeToString(sum[:])[:16]
}

// Entry is one workspace's registry record. Each `bones hub start`
// writes its own entry at ~/.bones/workspaces/<WorkspaceID>-<HubPID>.json
// — pid in the filename so two concurrent starts against the same
// workspace produce two entries (the duplicate-hub case from #208)
// rather than the second silently overwriting the first.
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
// workspace cwd and hub pid. The pid is encoded into the filename so
// concurrent hub starts against the same workspace produce distinct
// files (#208 acceptance criterion (c)).
func EntryPath(cwd string, pid int) string {
	return filepath.Join(RegistryDir(),
		fmt.Sprintf("%s-%d.json", WorkspaceID(cwd), pid))
}

// legacyEntryPath returns the pre-#208 unsuffixed path for backwards
// compatibility on read. Pre-fix the filename was just <id>.json
// regardless of pid; List still surfaces those entries so an upgrade
// across an operator's running hubs does not lose state.
func legacyEntryPath(cwd string) string {
	return filepath.Join(RegistryDir(), WorkspaceID(cwd)+".json")
}

// Write persists e to its file atomically (tmp+rename). Creates the registry
// directory if missing. Filename is keyed by (cwd, HubPID) — see EntryPath.
func Write(e Entry) error {
	if err := os.MkdirAll(RegistryDir(), 0o755); err != nil {
		return fmt.Errorf("registry mkdir: %w", err)
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("registry marshal: %w", err)
	}

	dst := EntryPath(e.Cwd, e.HubPID)
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

// Read loads the workspace's most-recent registry entry. If multiple
// entries exist (the duplicate-hub case), the alive one with the
// latest StartedAt wins; if none are alive, the latest StartedAt is
// returned. Use Duplicates(cwd) when the caller wants every entry.
func Read(cwd string) (Entry, error) {
	matches, err := matchingPaths(cwd)
	if err != nil {
		return Entry{}, err
	}
	if len(matches) == 0 {
		return Entry{}, ErrNotFound
	}
	var best Entry
	var bestSet bool
	for _, p := range matches {
		e, err := readEntryAtPath(p)
		if err != nil {
			continue
		}
		if !bestSet ||
			(pidAlive(e.HubPID) && !pidAlive(best.HubPID)) ||
			(pidAlive(e.HubPID) == pidAlive(best.HubPID) && e.StartedAt.After(best.StartedAt)) {
			best = e
			bestSet = true
		}
	}
	if !bestSet {
		return Entry{}, ErrNotFound
	}
	return best, nil
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

// List returns all registry entries, skipping corrupt files. Includes
// every per-pid entry so two concurrent hubs against one workspace
// surface as two list rows (caller decides whether to dedupe).
func List() ([]Entry, error) {
	matches, err := filepath.Glob(filepath.Join(RegistryDir(), "*.json"))
	if err != nil {
		return nil, fmt.Errorf("registry glob: %w", err)
	}
	out := make([]Entry, 0, len(matches))
	for _, path := range matches {
		// Skip atomic-write tmp files; only top-level *.json files are
		// real entries (matches the ListInfo skip).
		if strings.Contains(filepath.Base(path), ".tmp.") {
			continue
		}
		e, err := readEntryAtPath(path)
		if err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Remove deletes ALL registry entries for the given workspace cwd —
// every per-pid file plus the legacy unsuffixed path. Idempotent.
// Used by `bones down` and stale-entry pruners that operate at
// workspace granularity. Callers that want to drop a single entry by
// pid should use RemoveByPID instead.
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

// RemoveByPID deletes the single registry entry for (cwd, pid).
// Idempotent: missing files are not an error. Used by Reap and by
// foreground hub.Start's defer cleanup so a workspace with two live
// entries can lose one without forgetting the other.
func RemoveByPID(cwd string, pid int) error {
	err := os.Remove(EntryPath(cwd, pid))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("registry remove pid: %w", err)
}

// matchingPaths returns every file in RegistryDir that belongs to the
// given workspace cwd: the legacy <id>.json AND every per-pid
// <id>-<pid>.json. Result order is unspecified.
func matchingPaths(cwd string) ([]string, error) {
	id := WorkspaceID(cwd)
	dir := RegistryDir()
	// Glob for <id>-*.json (the per-pid scheme). Add the legacy
	// unsuffixed path explicitly so we cover both layouts on read.
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
	if _, err := os.Stat(legacyEntryPath(cwd)); err == nil {
		out = append(out, legacyEntryPath(cwd))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("registry stat legacy: %w", err)
	}
	return out, nil
}

// pidFromFilename parses the trailing -<pid>.json off a registry path
// and returns the pid. Returns (0, false) on any parse error or for
// the legacy <id>.json layout (no pid in filename). WorkspaceID is 16
// hex chars with no hyphens, so the first hyphen unambiguously
// separates id from pid.
func pidFromFilename(path string) (int, bool) {
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	idx := strings.Index(base, "-")
	if idx < 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(base[idx+1:])
	if err != nil {
		return 0, false
	}
	return pid, true
}
