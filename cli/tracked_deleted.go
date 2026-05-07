package cli

import (
	"fmt"
	"os/exec"
	"strings"
)

// maxTrackedDeletedListed bounds how many missing paths we print
// inline. Above this, the warning collapses to "... and N more" so
// the bones up output stays scannable on workspaces with large
// dirty trees.
const maxTrackedDeletedListed = 5

// trackedDeletedFiles returns paths in the git index that are
// missing from the working tree (deleted without `git rm`). Wraps
// `git ls-files --deleted`. Returns nil with no error when the
// directory is not a git repo or git is not on PATH — bones up
// already tolerates non-git workspaces.
func trackedDeletedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "--deleted")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		// Not a git repo, no git, or some other failure. Don't surface
		// — bones up is not the right place to nag about non-git state.
		return nil, nil
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// formatTrackedDeletedWarning renders the one-line WARN body for a
// non-empty set of missing-tracked paths. Truncates to the first
// maxTrackedDeletedListed entries to keep output scannable.
//
// #303 driver: surface dirty index state so the operator can either
// `git rm` the entries or restore them. Since #302 the hub itself
// tolerates this state, so this is informational rather than a
// blocker.
func formatTrackedDeletedWarning(missing []string) string {
	n := len(missing)
	listed := missing
	suffix := ""
	if n > maxTrackedDeletedListed {
		listed = missing[:maxTrackedDeletedListed]
		suffix = fmt.Sprintf(" ... and %d more",
			n-maxTrackedDeletedListed)
	}
	noun := "files are"
	if n == 1 {
		noun = "file is"
	}
	return fmt.Sprintf(
		"%d tracked %s missing from working tree (deleted without "+
			"'git rm'): %s%s",
		n, noun, strings.Join(listed, ", "), suffix)
}
