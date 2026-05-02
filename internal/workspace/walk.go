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

// FindRoot is the unauthenticated workspace lookup: walks up from start
// until it finds a directory containing the .bones marker dir, returns
// that directory, or ErrNoWorkspace if the filesystem root is reached.
//
// Unlike Join, FindRoot does not load config.json or contact the leaf
// daemon — useful for commands that only need the workspace path
// (e.g. bones apply, which materializes from the hub fossil and never
// talks to the leaf).
func FindRoot(start string) (string, error) {
	return walkUp(start)
}

// walkUp searches from start upward for a workspace marker directory
// (.bones/agent.id). Returns the workspace root, or ErrNoWorkspace if
// the filesystem root is reached without finding one.
//
// The agent.id marker (not just the .bones/ directory) is the canonical
// workspace signal: $HOME/.bones/ is a global state directory used for
// the cross-workspace registry and telemetry install-id, and matching
// it as a workspace would cause `bones status` and other workspace.Join
// callers to mistakenly resolve to $HOME when invoked from any
// subdirectory of $HOME without a closer .bones/agent.id marker. See
// issue #140.
func walkUp(start string) (string, error) {
	cur, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	for range maxWalkUpDepth {
		marker := filepath.Join(cur, markerDirName, agentIDFile)
		if _, err := os.Stat(marker); err == nil {
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
