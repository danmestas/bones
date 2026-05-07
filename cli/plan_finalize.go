package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	repocli "github.com/danmestas/EdgeSync/cli/repo"
	edgehub "github.com/danmestas/EdgeSync/hub"

	"github.com/danmestas/bones/internal/dispatch"
	"github.com/danmestas/bones/internal/workspace"
)

// PlanFinalizeCmd materializes files committed by each slot to hub
// trunk back into the host tree, closing the loop on the swarm
// workflow. The slot's `[slot: name]` annotation in the plan is the
// manifest — files listed in the dispatch manifest's per-slot
// `Files` are the materialization set. See ADR 0044.
type PlanFinalizeCmd struct {
	Plan  string `name:"plan" help:"plan path (default: read .bones/swarm/dispatch.json)"`
	Force bool   `name:"force" help:"overwrite host-tree files that differ from hub trunk"`
	Stage bool   `name:"stage" help:"git add the materialized files after writing"`
}

// finalizeResult holds the per-file outcomes of a finalize pass.
// Categories are mutually exclusive; sum equals total files visited.
type finalizeResult struct {
	Written    []string
	Matched    []string
	Conflicted []string
	Missing    []string // file in dispatch manifest but not on hub trunk
}

func (c *PlanFinalizeCmd) Run(g *repocli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("plan finalize: cwd: %w", err)
	}
	info, err := workspace.Join(context.Background(), cwd)
	if err != nil {
		return fmt.Errorf("plan finalize: %w", err)
	}
	return runPlanFinalize(c, info.WorkspaceDir, os.Stdout)
}

// runPlanFinalize is the testable entry point. workspaceDir is the
// resolved bones workspace root; out is where the summary prints.
func runPlanFinalize(c *PlanFinalizeCmd, workspaceDir string, out io.Writer) error {
	manifest, planSource, err := resolvePlanManifest(c.Plan, workspaceDir)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "plan finalize: %s\n", planSource)

	hubRepoPath := filepath.Join(workspace.BonesDir(workspaceDir), "hub.fossil")
	repo, err := edgehub.OpenRepo(hubRepoPath)
	if err != nil {
		return fmt.Errorf("plan finalize: open hub: %w", err)
	}
	defer func() { _ = repo.Close() }()

	res := materializeManifest(context.Background(), repo, manifest, workspaceDir, c.Force)
	printFinalizeSummary(out, res)

	if len(res.Conflicted) > 0 && !c.Force {
		return fmt.Errorf("plan finalize: %d file(s) conflict with host tree; "+
			"resolve manually or re-run with --force", len(res.Conflicted))
	}
	if c.Stage && len(res.Written) > 0 {
		if err := stageFiles(workspaceDir, res.Written); err != nil {
			return fmt.Errorf("plan finalize: --stage: %w", err)
		}
		_, _ = fmt.Fprintf(out, "plan finalize: staged %d file(s)\n", len(res.Written))
	}
	return nil
}

// resolvePlanManifest implements the ordering from ADR 0044: explicit
// --plan first, then .bones/swarm/dispatch.json, else error. Returns
// the manifest plus a one-line source description for the summary.
func resolvePlanManifest(planFlag, workspaceDir string) (
	*dispatch.Manifest, string, error,
) {
	if planFlag != "" {
		m, err := dispatch.BuildManifest(dispatch.BuildOptions{PlanPath: planFlag})
		if err != nil {
			return nil, "", fmt.Errorf("plan finalize: parse plan %s: %w", planFlag, err)
		}
		return &m, "plan=" + planFlag + " (--plan)", nil
	}
	m, err := dispatch.Read(workspaceDir)
	if err != nil {
		if errors.Is(err, dispatch.ErrNoManifest) {
			return nil, "", errors.New("plan finalize: no active dispatch found; " +
				"pass --plan=<path> to finalize a specific plan")
		}
		return nil, "", fmt.Errorf("plan finalize: read dispatch.json: %w", err)
	}
	return &m, "plan=" + m.PlanPath + " (active dispatch)", nil
}

// materializeManifest walks every (wave, slot, file) in the manifest,
// reads the file from hub trunk, and writes it to the host tree under
// the same relative path. Conflicts are listed without writing unless
// force is set; matched files (host == trunk) are reported and skipped.
func materializeManifest(
	ctx context.Context, repo *edgehub.Repo, m *dispatch.Manifest, workspaceDir string, force bool,
) finalizeResult {
	var res finalizeResult
	seen := map[string]bool{}
	for _, wave := range m.Waves {
		for _, slot := range wave.Slots {
			for _, file := range slot.Files {
				if seen[file] {
					continue
				}
				seen[file] = true
				trunk, err := repo.ReadAt(ctx, "trunk", file)
				if err != nil {
					res.Missing = append(res.Missing, file)
					continue
				}
				hostPath := filepath.Join(workspaceDir, file)
				if existing, err := os.ReadFile(hostPath); err == nil {
					if bytes.Equal(existing, trunk) {
						res.Matched = append(res.Matched, file)
						continue
					}
					if !force {
						res.Conflicted = append(res.Conflicted, file)
						continue
					}
				}
				if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
					res.Conflicted = append(res.Conflicted, file)
					continue
				}
				if err := os.WriteFile(hostPath, trunk, 0o644); err != nil {
					res.Conflicted = append(res.Conflicted, file)
					continue
				}
				res.Written = append(res.Written, file)
			}
		}
	}
	sort.Strings(res.Written)
	sort.Strings(res.Matched)
	sort.Strings(res.Conflicted)
	sort.Strings(res.Missing)
	return res
}

// printFinalizeSummary emits a per-category section so the operator
// sees what happened at a glance. Empty categories are still labeled
// (with "(none)") so the output shape is stable across runs.
func printFinalizeSummary(out io.Writer, res finalizeResult) {
	section := func(label string, files []string) {
		_, _ = fmt.Fprintf(out, "  %s (%d):\n", label, len(files))
		if len(files) == 0 {
			_, _ = fmt.Fprintln(out, "    (none)")
			return
		}
		for _, f := range files {
			_, _ = fmt.Fprintln(out, "    - "+f)
		}
	}
	section("written", res.Written)
	section("matched", res.Matched)
	section("conflicted", res.Conflicted)
	if len(res.Missing) > 0 {
		section("missing on trunk", res.Missing)
	}
}

// stageFiles runs `git add --` on every materialized file. Paths are
// joined explicitly to avoid shell-quoting hazards. A failure on any
// file aborts the stage; the operator can re-run after diagnosing.
func stageFiles(workspaceDir string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	args := append([]string{"add", "--"}, files...)
	cmd := exec.Command("git", args...)
	cmd.Dir = workspaceDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
