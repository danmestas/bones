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
// Default output: a single confirmation line. With -v / --verbose: the
// banner plus per-step status lines. WARN lines (drift, missing git)
// print regardless because they describe real issues the operator must
// see. Verbosity comes from the global -v flag on repocli.Globals.
type UpCmd struct{}

func (c *UpCmd) Run(g *repocli.Globals) error {
	if g.Verbose {
		banner.Print()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	return runUp(cwd, g.Verbose)
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
