package workspace

import (
	"fmt"
	"os"
	"path/filepath"
)

const markerDirName = ".agent-infra"

// walkUp searches from start upward for a directory containing markerDirName.
// Returns the path of the directory containing it, or ErrNoWorkspace if the
// filesystem root is reached without finding one.
func walkUp(start string) (string, error) {
	cur, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	for {
		candidate := filepath.Join(cur, markerDirName)
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", ErrNoWorkspace
		}
		cur = parent
	}
}
