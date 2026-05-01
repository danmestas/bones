package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WorkspaceID returns a deterministic 16-hex-char identifier for an absolute cwd.
// Used as the registry filename: ~/.bones/workspaces/<id>.json
func WorkspaceID(cwd string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return hex.EncodeToString(sum[:])[:16]
}

// Entry is one workspace's registry record. One JSON file per Entry at
// ~/.bones/workspaces/<WorkspaceID>.json.
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

// EntryPath returns the absolute path of the JSON file for the given workspace cwd.
func EntryPath(cwd string) string {
	return filepath.Join(RegistryDir(), WorkspaceID(cwd)+".json")
}

// Write persists e to its file atomically (tmp+rename). Creates the registry
// directory if missing.
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
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.Join(fmt.Errorf("registry write: %w", err), tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("registry sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("registry close: %w", err)
	}
	return os.Rename(tmp.Name(), dst)
}

// ErrNotFound is returned by Read when no entry exists for the given cwd.
var ErrNotFound = errors.New("registry: entry not found")

// Read loads the registry entry for the given workspace cwd.
func Read(cwd string) (Entry, error) {
	data, err := os.ReadFile(EntryPath(cwd))
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("registry read: %w", err)
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, fmt.Errorf("registry unmarshal: %w", err)
	}
	return e, nil
}

// List returns all registry entries, skipping corrupt files.
func List() ([]Entry, error) {
	matches, err := filepath.Glob(filepath.Join(RegistryDir(), "*.json"))
	if err != nil {
		return nil, fmt.Errorf("registry glob: %w", err)
	}
	out := make([]Entry, 0, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var e Entry
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Remove deletes the registry entry for the given workspace cwd. Idempotent.
func Remove(cwd string) error {
	err := os.Remove(EntryPath(cwd))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("registry remove: %w", err)
}
