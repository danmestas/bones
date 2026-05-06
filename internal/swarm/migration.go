package swarm

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ErrStaleClaudeWorktrees is returned by CheckStaleClaudeWorktrees
// when one or more legacy `.claude/worktrees/agent-*/` directories
// exist under the workspace root. ADR 0050 §"Migration: refuse-to-
// start on stale `.claude/worktrees/`": `bones up` and
// `bones hub start` refuse to proceed until those dirs are cleaned,
// because pre-ADR-0050 isolation no longer matches the synthetic
// slot machinery.
//
// Callers errors.Is-test this sentinel to map the failure to a
// non-zero CLI exit code that's distinct from the generic substrate
// errors.
var ErrStaleClaudeWorktrees = errors.New(
	"bones: refusing to start: legacy .claude/worktrees/agent-*/ dirs " +
		"present (run `bones cleanup --all-worktrees` to migrate; ADR 0050)",
)

// CheckStaleClaudeWorktrees globs for `<root>/.claude/worktrees/
// agent-*/` and returns ErrStaleClaudeWorktrees with the matching
// dirs listed in the message when one or more match. Returns nil
// when no matches exist (empty `.claude/worktrees/` dir is fine —
// only `agent-*` children are the migration trigger).
//
// Used by `bones up` and `bones hub start`. Other verbs deliberately
// skip the check: an operator may have legitimate reasons to inspect
// a stale dir on a workspace they're not actively running, and the
// loud-refusal point is the SessionStart bring-up, not every read-
// only verb.
//
// glob errors fall through as nil — a malformed pattern would be a
// programmer bug, not an operator-visible refusal trigger; the
// pattern is hardcoded so this can't happen in practice.
func CheckStaleClaudeWorktrees(root string) error {
	pattern := filepath.Join(root, ".claude", "worktrees", "agent-*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	if len(matches) == 0 {
		return nil
	}
	sort.Strings(matches)
	rels := make([]string, 0, len(matches))
	for _, m := range matches {
		if rel, err := filepath.Rel(root, m); err == nil {
			rels = append(rels, rel)
		} else {
			rels = append(rels, m)
		}
	}
	return fmt.Errorf("%w (found: %s)",
		ErrStaleClaudeWorktrees, strings.Join(rels, ", "))
}
