package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteName persists the workspace_name override at <root>/.bones/workspace_name.
func WriteName(root, name string) error {
	path := filepath.Join(root, ".bones", "workspace_name")
	return os.WriteFile(path, []byte(name+"\n"), 0o644)
}

// ReadName returns the workspace_name override or "" if not set.
func ReadName(root string) (string, error) {
	path := filepath.Join(root, ".bones", "workspace_name")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("workspace_name read: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
