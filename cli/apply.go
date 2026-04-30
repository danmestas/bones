package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/workspace"
)

// ApplyCmd materializes the hub fossil's trunk tip into the
// project-root git working tree and stages the changes for the user
// to review and commit. See
// docs/superpowers/specs/2026-04-30-bones-apply-design.md.
//
// bones apply never runs `git commit`. It writes files and stages with
// `git add -A` within fossil's tracked-paths set; the user owns the
// commit message and the commit author identity.
type ApplyCmd struct {
	DryRun bool `name:"dry-run" help:"show planned changes without writing or staging"`
}

func (c *ApplyCmd) Run(g *libfossilcli.Globals) error {
	return errors.New("bones apply: not yet implemented")
}

// applyPreflight is the resolved precondition state, returned by
// runApplyPreflight when every check passes.
type applyPreflight struct {
	WorkspaceDir string
	HubFossil    string
	FossilBin    string
}

// runApplyPreflight checks that the bones workspace, hub fossil, git
// repo, and system fossil binary are all in place. Returns the resolved
// paths or a user-facing error suitable for direct return from Run.
//
// Uses workspace.FindRoot rather than workspace.Join: bones apply only
// needs the workspace path, not a live leaf.
func runApplyPreflight(cwd string) (*applyPreflight, error) {
	root, err := workspace.FindRoot(cwd)
	if err != nil {
		return nil, fmt.Errorf("workspace not found: run `bones init` or `bones up` first (%w)", err)
	}
	hubRepo := filepath.Join(root, ".orchestrator", "hub.fossil")
	if _, err := os.Stat(hubRepo); err != nil {
		return nil, fmt.Errorf("hub repo not found at %s — run `bones up` first", hubRepo)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil, fmt.Errorf("no git repo at %s — bones apply requires git for staging", root)
	}
	fossilBin, err := exec.LookPath("fossil")
	if err != nil {
		return nil, errors.New(
			"bones apply requires the system `fossil` binary; install via " +
				"`brew install fossil` (or apt) and re-run",
		)
	}
	return &applyPreflight{
		WorkspaceDir: root,
		HubFossil:    hubRepo,
		FossilBin:    fossilBin,
	}, nil
}
