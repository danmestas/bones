package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/danmestas/bones/internal/githook"
	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/scaffoldver"
	"github.com/danmestas/bones/internal/telemetry"
	"github.com/danmestas/bones/internal/workspace"
)

// runUp performs workspace bootstrap from a fresh clone:
//  1. workspace init (idempotent — joins if already initialized)
//  2. orchestrator scaffold (skills, hooks, gitignore, scaffold version)
//  3. git pre-commit hook install
//  4. agent guidance write
//  5. Fossil drift check (warning only)
//
// Per ADR 0041 the hub is no longer started here. Any verb that needs the
// hub auto-starts it lazily via workspace.Join.
//
// Per ADR 0046 (#146), `bones up` is also the recovery path for a
// half-installed workspace: when step 1 has previously succeeded but
// step 2 did not (`.bones/agent.id` present, `.bones/scaffold_version`
// absent), runUp announces the recovery on stderr and re-runs scaffold
// idempotently. Each scaffold step is safe to call against partial
// state.
//
// Default output is a single confirmation line. With verbose=true, prints
// per-step status lines. WARN lines from drift / missing-git checks
// always print because they describe real issues operators must see.
func runUp(cwd string, verbose bool) (err error) {
	ctx, end := telemetry.RecordCommand(context.Background(), "bones.up",
		telemetry.String("workspace_hash", telemetry.WorkspaceHash(cwd)),
	)
	defer func() { end(err) }()

	// Detect recovery state BEFORE init: agent.id present from a prior
	// run + missing stamp = step 2 failed last time. The announcement
	// lands once on stderr per #146 — silent recovery would leave the
	// user wondering why a re-run worked after the previous error.
	recovery := isIncompleteScaffold(cwd)

	info, err := initOrJoinWorkspace(ctx, cwd)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	wsDir := info.WorkspaceDir
	if verbose {
		fmt.Printf("up: workspace at %s\n", wsDir)
	}

	if recovery {
		fmt.Fprintln(os.Stderr,
			"bones: scaffold incomplete from prior run — re-running scaffold")
	}

	if err := scaffoldOrchestrator(wsDir); err != nil {
		return fmt.Errorf("orchestrator scaffold: %w", err)
	}
	if verbose {
		fmt.Println("up: orchestrator skills, hooks, and gitignore installed")
	}

	if err := installGitHook(wsDir, verbose); err != nil {
		return fmt.Errorf("git hook: %w", err)
	}

	if err := writeAgentGuidance(wsDir); err != nil {
		return fmt.Errorf("agent guidance: %w", err)
	}

	if err := checkFossilDrift(wsDir); err != nil {
		fmt.Fprintf(os.Stderr, "up: WARN  %v\n", err)
	}

	if verbose {
		fmt.Println("up: workspace ready. Run any verb (e.g., `bones tasks status`) " +
			"and the hub will start automatically; or run `bones hub start` now.")
	} else {
		fmt.Printf("up: ready at %s\n", wsDir)
	}
	printHubStatus(os.Stdout, wsDir)
	return nil
}

// printHubStatus reflects the workspace's current hub state to w
// without spawning anything. Per ADR 0041 the hub starts lazily on
// the first verb that needs it, so a fresh `bones up` lands with no
// hub running and the operator otherwise has no signal that the
// scaffold actually completed (#138 item 11).
//
// Three shapes:
//
//	hub: running at <fossil-url> / <nats-url> (pid=N)
//	hub: previously recorded at <fossil-url> / <nats-url> — will
//	    restart on next verb
//	hub: not yet started — will start on next session (SessionStart
//	    hook) or first verb that needs it
//
// Read-only: no pid signaling, no spawning. Safe to call from any
// command that has just touched workspace state.
func printHubStatus(w io.Writer, root string) {
	fossilURL := hub.FossilURL(root)
	natsURL := hub.NATSURL(root)
	if fossilURL == "" || natsURL == "" {
		_, _ = fmt.Fprintln(w, "up: hub: not yet started — will start on "+
			"next session (SessionStart hook) or first verb that needs it")
		return
	}
	if pid, ok := hub.IsRunning(root); ok {
		_, _ = fmt.Fprintf(w, "up: hub: running at %s / %s (pid=%d)\n",
			fossilURL, natsURL, pid)
		return
	}
	_, _ = fmt.Fprintf(w, "up: hub: previously recorded at %s / %s — "+
		"will restart on next verb\n", fossilURL, natsURL)
}

// isIncompleteScaffold reports whether cwd lives inside a workspace
// whose `.bones/agent.id` marker exists but whose
// `.bones/scaffold_version` stamp is missing. This is the signature of
// a `bones up` that completed step 1 (workspace init) but failed step
// 2 (orchestrator scaffold), per #146 / ADR 0046.
//
// Returns false for fresh workspaces (no marker — nothing to recover
// from), for fully-scaffolded workspaces (stamp present), and when
// FindRoot fails. Used by runUp to decide whether to print the
// recovery-announcement line.
func isIncompleteScaffold(cwd string) bool {
	root, err := workspace.FindRoot(cwd)
	if err != nil {
		return false
	}
	stamp, _ := scaffoldver.Read(root)
	return stamp == ""
}

// initOrJoinWorkspace returns workspace.Info for cwd, creating the
// workspace if needed. New workspace → Init. Existing workspace → Join.
// This replaces an earlier ensureWorkspaceDir helper that only mkdir'd
// the marker dir and left config.json unwritten, which made every fresh
// `bones up` produce a workspace that workspace.Join couldn't load.
func initOrJoinWorkspace(ctx context.Context, cwd string) (workspace.Info, error) {
	info, err := workspace.Init(ctx, cwd)
	if errors.Is(err, workspace.ErrAlreadyInitialized) {
		return workspace.Join(ctx, cwd)
	}
	return info, err
}

// installGitHook installs the bones pre-commit hook in the host
// repository's .git/hooks directory. Per ADR 0034, this is the
// enforcement seam that prevents agents from silently bypassing the
// shadow trunk.
//
// The "no .git found" line prints regardless of verbose because it
// signals a missing enforcement gate the operator should know about.
// The success line is verbose-only.
func installGitHook(wsDir string, verbose bool) error {
	gitDir := githook.FindGitDir(wsDir)
	if gitDir == "" {
		fmt.Println("up: no .git found — skipping pre-commit hook install")
		return nil
	}
	if err := githook.Install(gitDir); err != nil {
		return err
	}
	if verbose {
		fmt.Printf("up: pre-commit hook installed at %s/hooks/pre-commit\n", gitDir)
	}
	return nil
}

// writeAgentGuidance creates .bones/AGENT_GUIDANCE.md so subagents
// that don't read CLAUDE.md still pick up workspace-level direction
// to use bones rather than direct git. The SessionStart hook reads
// this file and injects it into agent context.
func writeAgentGuidance(wsDir string) error {
	path := filepath.Join(wsDir, ".bones", "AGENT_GUIDANCE.md")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(agentGuidance), 0o644)
}

const agentGuidance = `# Bones is active in this workspace

A bones leaf is running. Commits go through bones, not direct git.

## What that means for you

If you are about to commit work, use:

    bones swarm commit -m "your message"

Do **not** run ` + "`git commit`" + ` or ` + "`git push`" + ` directly. The
pre-commit hook will refuse you, and the right answer is to fix
your workflow rather than bypass.

If bones state looks stale (fossil tip behind git HEAD, hub
unreachable, etc.), run:

    bones doctor

and report what it says. **Do not silently bypass.** If you are
absolutely certain the bypass is correct (rare), use
` + "`git commit --no-verify`" + ` so the override is explicit and
audited in your tool-call history.

## Why bones exists

Bones linearizes concurrent agent commits onto a single trunk via
autosync, then gates apply to your git tree behind a user sign-off.
Skipping bones loses both properties: parallel agents collide, and
unreviewed commits leak directly to your branches.

See ADR 0034 (` + "`docs/adr/0034-bypass-prevention.md`" + ` in the bones
repo) for the full rationale.
`

// checkFossilDrift compares the bones trunk fossil's tip against
// git HEAD. If they differ, it returns a non-fatal error suitable
// for surfacing as a warning. Per ADR 0034 §5: a future iteration
// will auto-seed; for now we surface the drift so the operator
// knows before they commit.
func checkFossilDrift(wsDir string) error {
	gitHead, err := readGitHead(wsDir)
	if err != nil {
		return nil
	}
	fossilTip := readFossilTip(wsDir)
	if fossilTip == "" {
		return nil
	}
	if gitHead == fossilTip {
		return nil
	}
	return fmt.Errorf("fossil tip (%s) != git HEAD (%s) — run `bones doctor` for details",
		shortHash(fossilTip), shortHash(gitHead))
}

func readGitHead(wsDir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = wsDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	hash := string(out)
	for len(hash) > 0 && (hash[len(hash)-1] == '\n' || hash[len(hash)-1] == ' ') {
		hash = hash[:len(hash)-1]
	}
	return hash, nil
}

// readFossilTip reads the fossil trunk tip recorded by bones. Returns
// empty string if the marker file doesn't exist (fresh workspace) or
// is unreadable. The marker is written by the leaf when it advances
// the trunk; reading it here keeps this package free of a fossil
// dependency.
func readFossilTip(wsDir string) string {
	path := filepath.Join(wsDir, ".bones", "trunk_tip")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	tip := string(data)
	for len(tip) > 0 && (tip[len(tip)-1] == '\n' || tip[len(tip)-1] == ' ') {
		tip = tip[:len(tip)-1]
	}
	return tip
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
