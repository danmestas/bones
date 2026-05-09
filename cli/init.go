package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/banner"
	"github.com/danmestas/bones/internal/workspace"
)

// InitCmd creates a new bones workspace in the current directory.
type InitCmd struct{}

func (c *InitCmd) Run(g *repocli.Globals) error {
	banner.Print()
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	info, err := workspace.Init(context.Background(), cwd)
	return reportWorkspace("init", info, err)
}

// JoinCmd locates and verifies an existing workspace from cwd.
type JoinCmd struct{}

func (c *JoinCmd) Run(g *repocli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	info, err := workspace.Join(context.Background(), cwd)
	return reportWorkspace("join", info, err)
}

// UpCmd performs workspace bootstrap from a fresh clone: workspace init
// (idempotent), orchestrator scaffold (scripts, skills, hooks), git hook
// install, agent guidance, and Fossil drift check. The hub itself is no
// longer started by `bones up`; per ADR 0041 it auto-starts on the first
// verb that needs it via workspace.Join.
//
// Per #314 the default output is now per-action structured: every
// gitignore add, hook install/rewrite, skill sync, and manifest bump
// gets one grep-friendly line, terminated by a one-line success
// signature ("up <workspace> actions=<n>"). --quiet suppresses both
// the per-action lines AND the summary signature; --json emits the
// same actions wrapped in the ADR 0053 schema envelope.
//
// --stealth (issue #291) suppresses the merge into .claude/settings.json
// — useful when running bones against a project where the operator does
// not want bones-managed hook entries written into Claude config.
// Combine with BONES_DIR=/some/path for a zero-workspace-write install.
type UpCmd struct {
	// Stealth skips the .claude/settings.json merge. Combine with
	// BONES_DIR=/path for a zero-workspace-write install.
	Stealth bool `name:"stealth" help:"skip .claude/settings.json merge (combine with BONES_DIR)"`
	JSON    bool `name:"json" help:"emit JSON envelope (ADR 0053)"`
	Quiet   bool `name:"quiet" help:"suppress per-action output and success signature"`
}

func (c *UpCmd) Run(g *repocli.Globals) error {
	if g.Verbose && !c.JSON {
		banner.Print()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	return runUp(cwd, upOpts{
		Verbose: g.Verbose,
		Stealth: c.Stealth,
		JSON:    c.JSON,
		Quiet:   c.Quiet,
	})
}

// reportWorkspace formats the standard workspace report and returns nil
// or a wrapped error suitable for Kong's exit code.
func reportWorkspace(op string, info workspace.Info, err error) error {
	if err == nil {
		fmt.Printf("workspace=%s\nagent_id=%s\nnats_url=%s\nleaf_http_url=%s\n",
			info.WorkspaceDir, info.AgentID, info.NATSURL, info.LeafHTTPURL)
		return nil
	}
	switch {
	case errors.Is(err, workspace.ErrAlreadyInitialized):
		fmt.Fprintln(os.Stderr,
			"workspace already initialized; run `bones join` instead")
	case errors.Is(err, workspace.ErrNoWorkspace):
		fmt.Fprintln(os.Stderr,
			"no bones workspace found; run `bones init` first")
	case errors.Is(err, workspace.ErrLeafStartTimeout):
		fmt.Fprintln(os.Stderr, "leaf failed to start within timeout")
	case errors.Is(err, workspace.ErrLegacyLayout):
		fmt.Fprintln(os.Stderr,
			"bones workspace uses pre-ADR-0041 layout and a leaf is currently running.\n"+
				"Tear it down first: `bones down`, then re-run to migrate.")
	}
	return fmt.Errorf("%s: %w", op, err)
}
