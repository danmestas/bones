package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/workspace"
)

// SwarmFanInCmd merges every open leaf on the hub repo's trunk into a
// single integration tip. After Phase 1 swarm slots close, the hub
// typically has N leaves (one per slot lineage) — fan-in collapses
// them so a downstream `fossil update` materializes a single merged
// working tree.
//
// Implementation shells out to the system `fossil` binary because
// libfossil's Repo.Merge requires distinct src/dst branch names, but
// concurrent slot work produces sibling leaves on the SAME branch
// (typically trunk). Same-branch leaf merging needs the multi-step
// orchestration fossil's CLI does (open temp checkout → fossil merge
// <UUID> → fossil commit). Acceptable: fan-in is an admin-scale
// operation invoked once per swarm, not a hot path; the same `fossil`
// dependency is already used by `bones peek`.
//
// On systems without `fossil` on PATH, `bones swarm fan-in` prints an
// install hint and exits non-zero so a wrapping orchestrator can fall
// back to manual instructions.
type SwarmFanInCmd struct {
	User    string `name:"user" default:"orchestrator" help:"fossil user attributed to merge"`
	Message string `name:"message" short:"m" default:"" help:"merge commit message"`
	DryRun  bool   `name:"dry-run" help:"show what would be merged without committing"`
}

func (c *SwarmFanInCmd) Run(g *libfossilcli.Globals) error {
	hubRepo, fossilBin, err := c.resolvePrereqs()
	if err != nil {
		return err
	}
	leaves, err := openLeavesOnTrunk(fossilBin, hubRepo)
	if err != nil {
		return fmt.Errorf("list open leaves: %w", err)
	}
	switch len(leaves) {
	case 0:
		fmt.Println("swarm fan-in: no open leaves on trunk; nothing to do")
		return nil
	case 1:
		fmt.Printf("swarm fan-in: only one leaf on trunk (%s); nothing to merge\n", leaves[0])
		return nil
	}
	if c.DryRun {
		printFanInDryRun(leaves)
		return nil
	}
	return c.mergeLeaves(fossilBin, hubRepo, leaves)
}

// resolvePrereqs validates the workspace exists, the hub repo is on
// disk, and the system `fossil` binary is reachable. Returns the hub
// repo path and the resolved fossil binary path or an error suitable
// for direct return from Run.
func (c *SwarmFanInCmd) resolvePrereqs() (string, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("cwd: %w", err)
	}
	info, err := workspace.Join(context.Background(), cwd)
	if err != nil {
		return "", "", fmt.Errorf("workspace: %w (run `bones init` or `bones up` first)", err)
	}
	hubRepo := filepath.Join(info.WorkspaceDir, ".orchestrator", "hub.fossil")
	if _, err := os.Stat(hubRepo); err != nil {
		return "", "", fmt.Errorf("hub repo not found at %s — run `bones up` first", hubRepo)
	}
	fossilBin, lookErr := exec.LookPath("fossil")
	if lookErr != nil {
		return "", "", fmt.Errorf(
			"swarm fan-in requires the system `fossil` binary; install via " +
				"`brew install fossil` (or apt) and re-run",
		)
	}
	return hubRepo, fossilBin, nil
}

// mergeLeaves opens a temp checkout, walks every non-canonical leaf
// merging it into the canonical, then commits the result as the
// configured user.
func (c *SwarmFanInCmd) mergeLeaves(fossilBin, hubRepo string, leaves []string) error {
	canonical, others := leaves[0], leaves[1:]
	mergeMsg := c.Message
	if mergeMsg == "" {
		mergeMsg = fmt.Sprintf("swarm fan-in: merge %d leaves into trunk", len(others))
	}
	wt, err := os.MkdirTemp(filepath.Dir(hubRepo), ".bones-fanin-*")
	if err != nil {
		return fmt.Errorf("mkdir temp checkout: %w", err)
	}
	defer func() { _ = os.RemoveAll(wt) }()
	if err := runFossil(fossilBin, "open", "--force", hubRepo, "--workdir", wt); err != nil {
		return fmt.Errorf("open temp checkout at %s: %w", wt, err)
	}
	defer func() { _ = runFossilIn(fossilBin, wt, "close", "--force") }()
	if err := runFossilIn(fossilBin, wt, "update", canonical); err != nil {
		return fmt.Errorf("update to canonical %s: %w", canonical, err)
	}
	for _, leaf := range others {
		if err := runFossilIn(fossilBin, wt, "merge", leaf); err != nil {
			return fmt.Errorf("merge leaf %s: %w", leaf, err)
		}
	}
	if err := runFossilIn(
		fossilBin, wt, "commit", "--no-warnings",
		"--user", c.User, "-m", mergeMsg,
	); err != nil {
		return fmt.Errorf("commit fan-in: %w", err)
	}
	fmt.Printf("swarm fan-in: merged %d leaves into trunk\n", len(others))
	fmt.Printf("  canonical:  %s\n", canonical)
	for _, leaf := range others {
		fmt.Printf("  merged-in:  %s\n", leaf)
	}
	return nil
}

func printFanInDryRun(leaves []string) {
	fmt.Printf("swarm fan-in (dry-run): would merge %d leaves into trunk:\n", len(leaves)-1)
	for _, l := range leaves[1:] {
		fmt.Printf("  - %s\n", l)
	}
	fmt.Printf("  (canonical kept: %s)\n", leaves[0])
}

// openLeavesOnTrunk runs `fossil leaves -t trunk` and returns the
// resulting UUIDs in the order fossil reports (most-recent first).
// The first entry is treated as the canonical destination by the caller;
// the rest are merge sources.
func openLeavesOnTrunk(fossilBin, hubRepo string) ([]string, error) {
	out, err := exec.Command(fossilBin, "leaves", "-R", hubRepo).Output()
	if err != nil {
		return nil, err
	}
	var leaves []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// fossil leaves output: "(N) YYYY-MM-DD HH:MM:SS [uuid] comment ..."
		open := strings.Index(line, "[")
		closeIdx := strings.Index(line, "]")
		if open < 0 || closeIdx <= open {
			continue
		}
		leaves = append(leaves, line[open+1:closeIdx])
	}
	return leaves, nil
}

// fossilEnv ensures USER is set for fossil's "who am I" detection.
// Inherited subprocess envs may lack USER (sandboxed test runners,
// some daemons), in which case `fossil update` and `fossil merge`
// abort with "Cannot figure out who you are!" before any user-mode
// flag like --user can apply. The orchestrator is the only sensible
// default for fan-in operations.
func fossilEnv() []string {
	return append(os.Environ(), "USER=orchestrator")
}

func runFossil(fossilBin string, args ...string) error {
	cmd := exec.Command(fossilBin, args...)
	cmd.Env = fossilEnv()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runFossilIn(fossilBin, dir string, args ...string) error {
	cmd := exec.Command(fossilBin, args...)
	cmd.Dir = dir
	cmd.Env = fossilEnv()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
