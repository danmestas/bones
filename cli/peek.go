package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/workspace"
)

// PeekCmd opens the workspace's hub Fossil repo in the system fossil
// binary's web UI (timeline, branches, files). It is an *enhancement*,
// not a hard dependency: when `fossil` isn't on PATH, peek prints the
// hub repo path with a one-line install hint and exits cleanly.
//
// libfossil's embedded HTTP server (used by `bones hub start` to serve
// /xfer for sync) does not implement the rich Fossil web UI; peek
// shells out to the canonical `fossil ui` for that.
type PeekCmd struct {
	Port int    `name:"port" help:"bind the UI on this port (default: fossil chooses)"`
	Page string `name:"page" default:"timeline?y=ci&n=50" help:"fossil page to land on; e.g. 'timeline', 'brlist', 'dir'"`
}

func (c *PeekCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	info, err := workspace.Join(context.Background(), cwd)
	if err != nil {
		return fmt.Errorf("workspace: %w (run `bones init` first)", err)
	}

	hubRepo := filepath.Join(info.WorkspaceDir, ".orchestrator", "hub.fossil")
	if _, err := os.Stat(hubRepo); err != nil {
		return fmt.Errorf(
			"hub repo not found at %s — run `bones up` or `bones hub start` first",
			hubRepo,
		)
	}

	fossilBin, lookErr := exec.LookPath("fossil")
	if lookErr != nil {
		fmt.Printf("peek: install fossil to open the rich timeline UI:\n")
		fmt.Printf("  brew install fossil   # macOS / Linux (homebrew)\n")
		fmt.Printf("  apt install fossil    # Debian / Ubuntu\n")
		fmt.Printf("\n")
		fmt.Printf("hub repo: %s\n", hubRepo)
		fmt.Printf("(any Fossil-compatible tool can open this file directly)\n")
		return nil
	}

	args := []string{"ui", hubRepo}
	if c.Port > 0 {
		args = append(args, "--port", strconv.Itoa(c.Port))
	}
	if c.Page != "" {
		args = append(args, "--page", c.Page)
	}

	fmt.Printf("peek: %s ui %s\n", fossilBin, hubRepo)
	cmd := exec.Command(fossilBin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
