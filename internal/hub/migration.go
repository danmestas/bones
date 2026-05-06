package hub

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/danmestas/bones/internal/telemetry"
)

// ErrStaleClaudeWorktrees is returned by checkStaleClaudeWorktrees
// when one or more legacy `.claude/worktrees/agent-*/` directories
// exist under the workspace root. ADR 0050 §"Migration: refuse-to-
// start on stale `.claude/worktrees/`": `bones hub start` refuses to
// proceed until those dirs are cleaned, because pre-ADR-0050
// isolation no longer matches the synthetic slot machinery.
//
// Mirrors swarm.ErrStaleClaudeWorktrees: the duplication exists
// because hub cannot import swarm (swarm depends on workspace which
// depends on hub — import cycle), and the migration check has to
// gate hub.Start before any side effect lands. swarm's package keeps
// the canonical definition for the bones-up call site; hub keeps its
// own copy for the bones-hub-start call site.
var ErrStaleClaudeWorktrees = errors.New(
	"bones: refusing to start: legacy .claude/worktrees/agent-*/ dirs " +
		"present (run `bones cleanup --all-worktrees` to migrate; ADR 0050)",
)

// startStartTelemetry begins the parent-only Start telemetry span and
// returns the wrapped ctx + the end func the caller must defer. Pulled
// out so hub.Start stays under the funlen lint cap. The detached
// child re-enters Start with BONES_HUB_FOREGROUND=1 and would
// otherwise emit a daemon-lifetime span the parent already covered;
// for the child path returns the original ctx + a no-op end func.
func startStartTelemetry(
	ctx context.Context, isDetachChild bool, fossilURLPath string, detach bool,
) (context.Context, telemetry.EndFunc) {
	if isDetachChild {
		return ctx, func(error, ...telemetry.Attr) {}
	}
	urlRecorded := readURLFile(fossilURLPath) != ""
	return telemetry.RecordCommand(ctx, "hub.start",
		telemetry.Bool("detach", detach),
		telemetry.Bool("url_recorded", urlRecorded),
	)
}

// gateStaleWorktrees is the Start-side wrapper around
// checkStaleClaudeWorktrees. Skips the check on the detach re-entry
// (the parent already gated; double-checking would race on the
// in-flight cleanup). Pulled out so Start stays under the funlen
// lint cap.
func gateStaleWorktrees(root string, isDetachChild bool) error {
	if isDetachChild {
		return nil
	}
	return checkStaleClaudeWorktrees(root)
}

// checkStaleClaudeWorktrees globs for `<root>/.claude/worktrees/
// agent-*/` and returns ErrStaleClaudeWorktrees with the matching
// dirs listed in the message when one or more match. Returns nil
// when no matches exist.
//
// glob errors fall through as nil — a malformed pattern would be a
// programmer bug, not an operator-visible refusal trigger; the
// pattern is hardcoded so this can't happen in practice.
func checkStaleClaudeWorktrees(root string) error {
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
