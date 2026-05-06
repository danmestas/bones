package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// CleanupCmd is the prompt-cleanup verb that operators (or harness
// `SubagentStop` recipes) invoke to reap a single slot or a legacy
// `.claude/worktrees/agent-*/` dir without waiting for the hub's
// lease-TTL watcher to converge.
//
// Three mutually-exclusive modes:
//
//   - --slot=<name>          drop the slot's session record and remove
//     `.bones/swarm/<slot>/wt/`. Idempotent —
//     a missing slot is a no-op (exit 0) so
//     re-running after a SubagentStop hook
//     already fired converges silently.
//   - --worktree=<path>      remove a single legacy
//     `.claude/worktrees/agent-*/` dir. Force-
//     unlocks the git worktree first so a
//     crashed-agent lock file does not block
//     removal.
//   - --all-worktrees        remove the entire `.claude/worktrees/`
//     tree. Useful for the migration from
//     pre-ADR-0050 isolation; the loud
//     `bones up` refusal points operators here.
//
// Bones ships only the verb. The recipe for wiring this into Claude
// Code's `SubagentStop` hook is documented at
// `docs/recipes/claude-code-harness.md`; bones does NOT scaffold the
// hook because it does not own `.claude/settings.json` wholesale
// (#256, #260, ADR 0050).
type CleanupCmd struct {
	Slot         string `name:"slot" help:"reap a single slot (drops session record + removes wt)"`    //nolint:lll
	Worktree     string `name:"worktree" help:"remove a single legacy .claude/worktrees/agent-*/ dir"` //nolint:lll
	AllWorktrees bool   `name:"all-worktrees" help:"remove the entire .claude/worktrees/ tree"`        //nolint:lll
}

// Run dispatches on the chosen mode. Mutual exclusion is enforced
// up front so a typo'd combination fails before any side effect.
func (c *CleanupCmd) Run(g *repocli.Globals) error {
	if err := c.validateFlags(); err != nil {
		return err
	}
	if c.Slot != "" {
		return c.runSlot(c.Slot)
	}
	if c.Worktree != "" {
		return c.runWorktree(c.Worktree)
	}
	return c.runAllWorktrees()
}

// validateFlags enforces the mutual-exclusion invariant: exactly
// one mode flag must be set. Two flags → user is confused, refuse
// before either side effect lands. Zero flags → print a usage hint
// (the verb does nothing safe by default).
func (c *CleanupCmd) validateFlags() error {
	count := 0
	if c.Slot != "" {
		count++
	}
	if c.Worktree != "" {
		count++
	}
	if c.AllWorktrees {
		count++
	}
	if count == 0 {
		return errors.New(
			"bones cleanup: pass exactly one of --slot=<name>, " +
				"--worktree=<path>, or --all-worktrees",
		)
	}
	if count > 1 {
		return errors.New(
			"bones cleanup: --slot, --worktree, and --all-worktrees " +
				"are mutually exclusive",
		)
	}
	return nil
}

// runSlot reaps one slot. The verb is idempotent: a missing
// session record + missing wt directory is a no-op (exit 0). When
// the workspace itself is unreachable (no .bones/, no live hub) the
// verb falls back to filesystem-only cleanup so an operator can run
// `bones cleanup --slot=X` even after `bones down` has torn the
// hub state down.
func (c *CleanupCmd) runSlot(slot string) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		// Workspace not joinable — surface the same shape as
		// other verbs but make the no-op path explicit so the
		// SubagentStop hook converges.
		return fmt.Errorf("bones cleanup: %w", err)
	}
	defer stop()

	sess, closeSess, openErr := openSwarmSessions(ctx, info)
	if openErr != nil {
		// NATS unreachable: do best-effort filesystem cleanup
		// only. The session record will fall out of the bucket
		// via JetStream KV TTL even without our delete.
		fmt.Fprintf(os.Stderr,
			"bones cleanup: warning: %v (filesystem-only cleanup)\n",
			openErr,
		)
		return cleanupSlotFS(info.WorkspaceDir, slot)
	}
	defer closeSess()

	wt := swarm.SlotWorktree(info.WorkspaceDir, slot)
	hadWT := false
	if st, statErr := os.Stat(wt); statErr == nil && st.IsDir() {
		hadWT = true
	}

	existed, err := swarm.SlotCleanup(ctx, sess, info.WorkspaceDir, slot)
	if err != nil {
		return fmt.Errorf("bones cleanup: %w", err)
	}
	switch {
	case existed:
		fmt.Fprintf(os.Stderr, "bones cleanup: reaped slot %q\n", slot)
	case hadWT:
		// No KV record but a wt dir was sitting on disk — typical
		// when an operator manually mkdir'd a slot dir or when the
		// substrate already evicted the record but the host was
		// offline when it happened. Still call it a successful
		// removal (not a no-op).
		fmt.Fprintf(os.Stderr,
			"bones cleanup: removed slot %q wt dir (no live record)\n", slot)
	default:
		fmt.Fprintf(os.Stderr,
			"bones cleanup: slot %q already clean (no-op)\n", slot)
	}
	return nil
}

// cleanupSlotFS is the substrate-unreachable fallback: remove the
// slot's wt dir without touching KV. The TTL watcher (or the next
// `bones up`) will reap the orphan record.
func cleanupSlotFS(workspaceDir, slot string) error {
	wt := swarm.SlotWorktree(workspaceDir, slot)
	if err := os.RemoveAll(wt); err != nil {
		return fmt.Errorf("remove %s: %w", wt, err)
	}
	fmt.Fprintf(os.Stderr,
		"bones cleanup: removed %s (substrate offline; record may persist until TTL)\n",
		wt,
	)
	return nil
}

// runWorktree removes one legacy `.claude/worktrees/agent-*/` dir.
// Force-unlocks the git worktree first so a crashed-agent lock
// file (`.git/worktrees/<name>/locked`) does not block removal.
//
// Errors loudly when path does not exist OR when path is not a
// recognizable worktree dir — the operator should know they typo'd
// the path before discovering nothing happened.
func (c *CleanupCmd) runWorktree(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("bones cleanup: resolve %s: %w", path, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Idempotent: missing path is a no-op.
			fmt.Fprintf(os.Stderr,
				"bones cleanup: %s already gone (no-op)\n", abs)
			return nil
		}
		return fmt.Errorf("bones cleanup: stat %s: %w", abs, err)
	}
	if !st.IsDir() {
		return fmt.Errorf(
			"bones cleanup: %s is not a directory — pass --all-worktrees "+
				"to remove the entire .claude/worktrees/ tree, or use "+
				"`git worktree remove --force` for non-worktree paths",
			abs,
		)
	}
	// Best-effort `git worktree remove --force`. If git is missing
	// or the path is not registered as a git worktree (e.g. a stale
	// dir whose owning repo has been garbage-collected), fall
	// through to plain os.RemoveAll. The end state is the same
	// (path gone) regardless of which path got us there.
	tryGitWorktreeRemove(abs)
	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("bones cleanup: remove %s: %w", abs, err)
	}
	fmt.Fprintf(os.Stderr, "bones cleanup: removed worktree %s\n", abs)
	return nil
}

// tryGitWorktreeRemove invokes `git worktree remove --force <path>`
// from the parent of path, swallowing any error. The returned exit
// code is irrelevant — the caller falls through to os.RemoveAll
// either way. Pulled into a helper so runWorktree's flow reads
// linearly.
func tryGitWorktreeRemove(path string) {
	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	parent := filepath.Dir(path)
	cmd := exec.Command("git", "worktree", "remove", "--force", path)
	cmd.Dir = parent
	// Silence stderr/stdout: this is a best-effort assist, not the
	// authoritative remove. The os.RemoveAll below is.
	_ = cmd.Run()
}

// runAllWorktrees removes the entire `.claude/worktrees/` tree. No
// per-worktree git remove — a `bones up` refusal pointed the
// operator here, so the tree is by definition stale. Idempotent;
// missing tree is exit 0.
func (c *CleanupCmd) runAllWorktrees() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("bones cleanup: cwd: %w", err)
	}
	root := findClaudeWorktreesRoot(cwd)
	if root == "" {
		fmt.Fprintln(os.Stderr,
			"bones cleanup: no .claude/worktrees/ tree found (no-op)")
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("bones cleanup: remove %s: %w", root, err)
	}
	fmt.Fprintf(os.Stderr, "bones cleanup: removed %s\n", root)
	return nil
}

// findClaudeWorktreesRoot walks up from start to locate
// `<repo-root>/.claude/worktrees`. Returns "" when no such
// directory exists at any ancestor. Uses workspace.FindRoot when
// available (a bones workspace always has .bones/) and otherwise
// falls back to a self-contained walk so the verb still works in
// non-bones git repos that legacy .claude/worktrees/ dirs may live
// under.
func findClaudeWorktreesRoot(start string) string {
	if root, err := workspace.FindRoot(start); err == nil {
		candidate := filepath.Join(root, ".claude", "worktrees")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
	}
	// Fallback: scan up until we hit `.claude/worktrees` or the
	// filesystem root.
	cur := start
	for {
		candidate := filepath.Join(cur, ".claude", "worktrees")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}
