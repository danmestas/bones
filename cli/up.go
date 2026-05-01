package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/danmestas/bones/internal/githook"
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
func runUp(cwd string) (err error) {
	ctx, end := telemetry.RecordCommand(context.Background(), "bones.up",
		telemetry.String("workspace_hash", telemetry.WorkspaceHash(cwd)),
	)
	defer func() { end(err) }()

	info, err := initOrJoinWorkspace(ctx, cwd)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	wsDir := info.WorkspaceDir
	fmt.Printf("up: workspace at %s\n", wsDir)

	if err := scaffoldOrchestrator(wsDir); err != nil {
		return fmt.Errorf("orchestrator scaffold: %w", err)
	}
	fmt.Println("up: orchestrator skills, hooks, and gitignore installed")

	if err := installGitHook(wsDir); err != nil {
		return fmt.Errorf("git hook: %w", err)
	}

	if err := writeAgentGuidance(wsDir); err != nil {
		return fmt.Errorf("agent guidance: %w", err)
	}

	if err := checkFossilDrift(wsDir); err != nil {
		fmt.Fprintf(os.Stderr, "up: WARN  %v\n", err)
	}

	fmt.Println("up: workspace ready. Run any verb (e.g., `bones tasks status`) " +
		"and the hub will start automatically; or run `bones hub start` now.")
	return nil
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
func installGitHook(wsDir string) error {
	gitDir := githook.FindGitDir(wsDir)
	if gitDir == "" {
		fmt.Println("up: no .git found — skipping pre-commit hook install")
		return nil
	}
	if err := githook.Install(gitDir); err != nil {
		return err
	}
	fmt.Printf("up: pre-commit hook installed at %s/hooks/pre-commit\n", gitDir)
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
