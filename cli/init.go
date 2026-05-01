package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/banner"
	"github.com/danmestas/bones/internal/workspace"
)

// InitCmd creates a new bones workspace in the current directory.
type InitCmd struct{}

func (c *InitCmd) Run(g *libfossilcli.Globals) error {
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

func (c *JoinCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	info, err := workspace.Join(context.Background(), cwd)
	return reportWorkspace("join", info, err)
}

// OrchestratorCmd installs the hub-leaf orchestrator scripts, skills,
// and Claude Code hooks into an existing workspace.
type OrchestratorCmd struct{}

func (c *OrchestratorCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	info, err := workspace.Join(context.Background(), cwd)
	if err != nil {
		return fmt.Errorf("must be run inside a workspace (try `bones init` first): %w", err)
	}
	if err := scaffoldOrchestrator(info.WorkspaceDir); err != nil {
		return err
	}
	fmt.Println("orchestrator: scaffolded scripts, skills, and Claude Code hooks")
	return nil
}

// UpCmd performs full bootstrap from a fresh clone: workspace init,
// orchestrator scaffold, leaf binary resolution, and hub bootstrap.
type UpCmd struct{}

func (c *UpCmd) Run(g *libfossilcli.Globals) error {
	banner.Print()
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	return runUp(cwd)
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
