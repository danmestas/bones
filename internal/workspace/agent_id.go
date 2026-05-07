package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

const agentIDFile = "agent.id"

// writeAgentID stores the workspace's coord identity at <BonesDir>/agent.id.
// Creates the marker directory if missing. Caller must hold any lock.
func writeAgentID(workspaceDir, id string) error {
	markerDir := BonesDir(workspaceDir)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(markerDir, agentIDFile),
		[]byte(id+"\n"), 0o644)
}

// readAgentID reads the workspace's coord identity. Returns os.ErrNotExist
// when the workspace has not been initialized.
func readAgentID(workspaceDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(BonesDir(workspaceDir), agentIDFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
