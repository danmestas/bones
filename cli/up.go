package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/danmestas/bones/internal/githook"
	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/telemetry"
	"github.com/danmestas/bones/internal/workspace"
)

// runUp performs a full single-command bootstrap from a fresh clone:
//  1. workspace init (idempotent — joins if already initialized)
//  2. orchestrator scaffold (scripts, skills, hooks)
//  3. build bin/leaf if missing
//  4. run hub-bootstrap.sh and verify the hub is up
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
	fmt.Println("up: orchestrator scripts, skills, and hooks installed")

	if err := installGitHook(wsDir); err != nil {
		return fmt.Errorf("git hook: %w", err)
	}

	if err := writeAgentGuidance(wsDir); err != nil {
		return fmt.Errorf("agent guidance: %w", err)
	}

	if err := checkFossilDrift(wsDir); err != nil {
		fmt.Fprintf(os.Stderr, "up: WARN  %v\n", err)
	}

	if err := ensureLeafBinary(wsDir); err != nil {
		return fmt.Errorf("bin/leaf: %w", err)
	}

	if err := runHubBootstrap(wsDir); err != nil {
		return fmt.Errorf("hub-bootstrap: %w", err)
	}

	fossilURL := hub.FossilURL(wsDir)
	natsURL := hub.NATSURL(wsDir)
	if fossilURL == "" {
		return fmt.Errorf("hub started but no .orchestrator/hub-fossil-url recorded — " +
			"check .orchestrator/hub.log")
	}
	if err := waitHubReady(fossilURL+"/xfer", 5*time.Second); err != nil {
		return fmt.Errorf("hub health check: %w", err)
	}
	fmt.Printf("up: hub is up at %s — NATS at %s\n", fossilURL, natsURL)
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

// ensureLeafBinary checks the standard leaf-binary locations used by
// hub-bootstrap.sh. If the binary is missing it attempts to build it
// from the sibling EdgeSync repo. Matches hub-bootstrap.sh resolution
// order:
//  1. $LEAF_BIN env var
//  2. $ROOT/bin/leaf
//  3. $EDGESYNC_DIR/bin/leaf  ($EDGESYNC_DIR defaults to $ROOT/../EdgeSync)
//  4. `leaf` on $PATH
//  5. Build from $EDGESYNC_DIR/leaf/cmd/leaf
func ensureLeafBinary(root string) error {
	if p := os.Getenv("LEAF_BIN"); p != "" {
		if isExec(p) {
			fmt.Printf("up: using LEAF_BIN=%s\n", p)
			return nil
		}
		return fmt.Errorf("LEAF_BIN=%s is not executable", p)
	}

	rootLeaf := filepath.Join(root, "bin", "leaf")
	if isExec(rootLeaf) {
		fmt.Printf("up: found bin/leaf at %s\n", rootLeaf)
		return nil
	}

	edgesyncDir := os.Getenv("EDGESYNC_DIR")
	if edgesyncDir == "" {
		edgesyncDir = filepath.Join(root, "..", "EdgeSync")
	}
	edgesyncLeaf := filepath.Join(edgesyncDir, "bin", "leaf")
	if isExec(edgesyncLeaf) {
		fmt.Printf("up: found leaf at %s\n", edgesyncLeaf)
		return nil
	}

	if p, err := exec.LookPath("leaf"); err == nil {
		fmt.Printf("up: found leaf on PATH: %s\n", p)
		return nil
	}

	leafSrc := filepath.Join(edgesyncDir, "leaf", "cmd", "leaf")
	if _, err := os.Stat(leafSrc); os.IsNotExist(err) {
		return fmt.Errorf(
			"EdgeSync sibling clone not found at %s — "+
				"clone https://github.com/danmestas/EdgeSync next to bones, "+
				"then re-run `bones up`",
			edgesyncDir,
		)
	}
	fmt.Printf("up: building bin/leaf in %s ...\n", edgesyncDir)
	cmd := exec.Command("make", "leaf")
	cmd.Dir = edgesyncDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("make leaf in %s: %w", edgesyncDir, err)
	}
	if !isExec(edgesyncLeaf) {
		return fmt.Errorf("make leaf succeeded but %s still not executable", edgesyncLeaf)
	}
	fmt.Printf("up: built %s\n", edgesyncLeaf)
	return nil
}

// runHubBootstrap execs hub-bootstrap.sh from the workspace root.
// The script is idempotent.
func runHubBootstrap(root string) error {
	script := filepath.Join(root, ".orchestrator", "scripts", "hub-bootstrap.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("hub-bootstrap.sh not found at %s "+
			"(run orchestrator scaffold first)", script)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("hub-bootstrap.sh: %w", err)
	}
	return nil
}

// waitHubReady polls the hub's /xfer endpoint until it responds or the
// timeout elapses. A 405 (method not allowed) counts as "up" — /xfer
// only accepts POST.
func waitHubReady(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx // fire-and-forget health probe
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusMethodNotAllowed {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("hub at %s did not become ready within %s", url, timeout)
}

func isExec(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&0o100 != 0
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
