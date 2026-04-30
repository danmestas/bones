package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

// trunkManifest returns the list of files tracked at the hub fossil's
// trunk tip and the tip's hex rev. Shells to the system fossil binary,
// matching the pattern in cli/swarm_fanin.go.
func trunkManifest(hubFossil, fossilBin string) ([]string, string, error) {
	paths, err := manifestAtRev(hubFossil, fossilBin, "trunk")
	if err != nil {
		return nil, "", err
	}
	rev, err := trunkRev(hubFossil, fossilBin)
	if err != nil {
		return paths, "", err
	}
	return paths, rev, nil
}

// dirtyTrackedPaths returns the subset of fossil-manifest paths that
// have staged or unstaged modifications in the workspace's git tree.
// Untracked-by-fossil files are not consulted regardless of their git
// state — the apply contract is "refuse if fossil would clobber the
// user's work," not "refuse if anything is dirty."
func dirtyTrackedPaths(workspaceDir string, manifest []string) ([]string, error) {
	if len(manifest) == 0 {
		return nil, nil
	}
	manifestSet := make(map[string]struct{}, len(manifest))
	for _, p := range manifest {
		manifestSet[p] = struct{}{}
	}
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=no")
	cmd.Dir = workspaceDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	var dirty []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: "XY <path>" where X = index status, Y = worktree status.
		path := strings.TrimSpace(line[3:])
		// Rename lines have "old -> new"; take the new name.
		if idx := strings.LastIndex(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		if _, ok := manifestSet[path]; ok {
			dirty = append(dirty, path)
		}
	}
	return dirty, nil
}

// manifestAtRev lists files at a specific rev (hex UUID or symbolic
// name like "trunk"). `-r` is required so `fossil ls` runs against the
// repo without a live checkout — without `-r`, fossil ls expects to be
// run inside a fossil working directory.
func manifestAtRev(hubFossil, fossilBin, rev string) ([]string, error) {
	out, err := exec.Command(fossilBin, "ls", "-R", hubFossil, "-r", rev).Output()
	if err != nil {
		return nil, fmt.Errorf("fossil ls @ %s: %w", rev, err)
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// trunkRev returns the trunk tip's hex UUID via `fossil info`.
// Accepts both legacy (`uuid:`) and current (`hash:`) labels.
func trunkRev(hubFossil, fossilBin string) (string, error) {
	out, err := exec.Command(fossilBin, "info", "-R", hubFossil, "trunk").Output()
	if err != nil {
		return "", fmt.Errorf("fossil info trunk: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"uuid:", "hash:"} {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix)), nil
			}
		}
	}
	return "", errors.New("could not parse trunk rev from `fossil info`")
}

