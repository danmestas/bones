package telemetry

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// installIDFile is the path (relative to the user's home directory)
// where the opaque per-install UUID is persisted. Same install across
// many workspaces produces the same ID; uninstalling and reinstalling
// produces a new one (manual deletion of ~/.bones/install-id resets it).
const installIDFile = ".bones/install-id"

// InstallID returns the opaque per-install identifier from
// ~/.bones/install-id, generating and persisting a fresh UUIDv4 if the
// file does not yet exist. Returns "" on I/O error — telemetry must
// never block or fail a bones operation.
//
// The ID is meaningful only to the operator's own SigNoz backend: it
// lets aggregations like "8% of installs hit hub-bind error in v0.4.1"
// be answered without identifying any individual user. Hostname,
// username, and path are deliberately not included anywhere.
func InstallID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, installIDFile)
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}
	id := uuid.NewString()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return id
	}
	_ = os.WriteFile(path, []byte(id+"\n"), 0o644)
	return id
}
