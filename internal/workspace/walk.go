package workspace

import (
	"fmt"
	"os"
	"path/filepath"
)

const markerDirName = ".bones"

// maxWalkUpDepth caps the number of directory levels walkUp will ascend.
// 64 levels is preposterous for any real filesystem; exceeding it indicates
// a runtime or filesystem anomaly.
const maxWalkUpDepth = 64

// walkUp searches from start upward for a directory containing markerDirName.
// Returns the path of the directory containing it, or ErrNoWorkspace if the
// filesystem root is reached without finding one.
func walkUp(start string) (string, error) {
	cur, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	for i := 0; i < maxWalkUpDepth; i++ {
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
	return "", fmt.Errorf("workspace: walkUp exceeded %d levels", maxWalkUpDepth)
}
