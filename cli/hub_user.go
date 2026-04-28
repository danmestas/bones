package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/danmestas/libfossil"
	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/workspace"
)

// HubUserCmd groups subcommands that manage the fossil user table on
// the hub repo (.orchestrator/hub.fossil). The table is consulted by
// every `fossil commit --user X` against that repo, so swarm slots that
// commit under their own identity (slot-rendering, slot-physics, etc.)
// must exist here first or fossil rejects the commit with "no such
// user."
//
// This primitive is the manual escape hatch; future swarm-join tooling
// will call into the same internal helpers automatically.
type HubUserCmd struct {
	Add  HubUserAddCmd  `cmd:"" help:"Add (or noop-if-exists) a user to the hub repo"`
	List HubUserListCmd `cmd:"" help:"List users in the hub repo"`
}

// HubUserAddCmd creates a user in the hub repo. Idempotent: if the user
// already exists, exits 0 without changing anything.
type HubUserAddCmd struct {
	Login string `arg:"" help:"login (e.g. slot-rendering)"`
	Caps  string `name:"caps" default:"oih" help:"fossil caps (default: clone+checkin+history)"`
}

func (c *HubUserAddCmd) Run(g *libfossilcli.Globals) error {
	repoPath, err := hubRepoPath()
	if err != nil {
		return err
	}
	repo, err := libfossil.Open(repoPath)
	if err != nil {
		return fmt.Errorf("open hub repo: %w", err)
	}
	defer func() { _ = repo.Close() }()

	if _, err := repo.GetUser(c.Login); err == nil {
		fmt.Printf("hub user %q already exists (no change)\n", c.Login)
		return nil
	}

	if err := repo.CreateUser(libfossil.UserOpts{
		Login: c.Login,
		Caps:  c.Caps,
	}); err != nil {
		return fmt.Errorf("create user %q: %w", c.Login, err)
	}
	fmt.Printf("hub user %q created (caps=%s)\n", c.Login, c.Caps)
	return nil
}

// HubUserListCmd prints the hub repo's user table.
type HubUserListCmd struct{}

func (c *HubUserListCmd) Run(g *libfossilcli.Globals) error {
	repoPath, err := hubRepoPath()
	if err != nil {
		return err
	}
	repo, err := libfossil.Open(repoPath)
	if err != nil {
		return fmt.Errorf("open hub repo: %w", err)
	}
	defer func() { _ = repo.Close() }()

	users, err := repo.ListUsers()
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	for _, u := range users {
		fmt.Printf("%-30s caps=%s\n", u.Login, u.Caps)
	}
	return nil
}

// hubRepoPath finds the workspace from cwd and returns the hub repo path.
// Surfaces clear errors when the workspace or repo isn't there yet.
func hubRepoPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	info, err := workspace.Join(context.Background(), cwd)
	if err != nil {
		return "", fmt.Errorf("workspace: %w (run `bones init` or `bones up` first)", err)
	}
	repoPath := filepath.Join(info.WorkspaceDir, ".orchestrator", "hub.fossil")
	if _, err := os.Stat(repoPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf(
				"hub repo not found at %s — run `bones up` or `bones hub start` first",
				repoPath,
			)
		}
		return "", fmt.Errorf("stat hub repo: %w", err)
	}
	return repoPath, nil
}
