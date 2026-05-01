package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Marker is a per-session record. One JSON file per Marker at
// ~/.bones/sessions/<SessionID>.json.
type Marker struct {
	SessionID    string    `json:"session_id"`
	WorkspaceCwd string    `json:"workspace_cwd"`
	ClaudePID    int       `json:"claude_pid"`
	StartedAt    time.Time `json:"started_at"`
}

// SessionsDir returns the directory holding session marker files.
func SessionsDir() string {
	return filepath.Join(os.Getenv("HOME"), ".bones", "sessions")
}

// MarkerPath returns the file path for a given session ID.
func MarkerPath(sessionID string) string {
	return filepath.Join(SessionsDir(), sessionID+".json")
}

// Register persists m atomically (tmp+rename).
func Register(m Marker) error {
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		return fmt.Errorf("sessions mkdir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marker marshal: %w", err)
	}
	dst := MarkerPath(m.SessionID)
	tmp, err := os.CreateTemp(SessionsDir(), filepath.Base(dst)+".tmp.*")
	if err != nil {
		return fmt.Errorf("marker tmp: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("marker write: %w", err), closeErr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("marker sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("marker close: %w", err)
	}
	return os.Rename(tmp.Name(), dst)
}
