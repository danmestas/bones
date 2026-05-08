package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/cli/schemas"
	"github.com/danmestas/bones/cli/uxprint"
	"github.com/danmestas/bones/internal/dispatch"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

// SwarmDispatchCmd implements `bones swarm dispatch`.
//
// Without a flag, it reads a plan file, creates one task per slot via the
// bones-tasks KV, writes a dispatch manifest, and prints the manifest path
// with a next-step instruction for the orchestrator skill.
//
// --advance checks whether all tasks in the current wave are Closed; if so
// it promotes CurrentWave to the next wave number and rewrites the manifest.
//
// --cancel removes the manifest and closes any referenced tasks with
// ClosedReason="dispatch-canceled".
//
// --dry-run validates the plan (or checks advance/cancel preconditions)
// without touching NATS or the filesystem.
type SwarmDispatchCmd struct {
	PlanPath string `arg:"" optional:"" name:"plan" help:"path to plan markdown"`
	Advance  bool   `name:"advance" help:"check current wave; promote if all tasks Closed"`
	Cancel   bool   `name:"cancel" help:"abandon in-flight dispatch; closes tasks as canceled"`
	Wave     int    `name:"wave" help:"explicit wave number (rare; for testing)"`
	JSON     bool   `name:"json" help:"emit manifest path + summary as JSON"`
	DryRun   bool   `name:"dry-run" help:"validate; don't touch NATS or filesystem"`
	Quiet    bool   `name:"quiet" help:"suppress success output"`
}

// Run dispatches to the appropriate subcommand based on which flag is set.
func (c *SwarmDispatchCmd) Run(g *repocli.Globals) error {
	// Validate mode before touching the workspace so the usage error is fast.
	if !c.Cancel && !c.Advance && c.PlanPath == "" {
		return fmt.Errorf("usage: bones swarm dispatch <plan> | --advance | --cancel")
	}

	ctx := context.Background()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	info, err := workspace.Join(ctx, cwd)
	if err != nil {
		return err
	}
	switch {
	case c.Cancel:
		return c.runCancel(ctx, info)
	case c.Advance:
		return c.runAdvance(ctx, info)
	default:
		return c.runDispatch(ctx, info, c.PlanPath)
	}
}

// dispatchSummaryJSON was the pre-ADR-0053 --json output shape; the
// emitter now constructs schemas.SwarmDispatchPayload directly so
// the typed struct lives in one place. Kept removed deliberately —
// reintroducing it would re-fork the contract.

// planSHA256 computes the hex-encoded SHA-256 of a plan file's bytes.
// Matches the hash BuildManifest stores so re-dispatch detection is consistent.
func planSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// runDispatch validates the plan, creates tasks, builds and writes the
// manifest, then prints a summary or --json output.
func (c *SwarmDispatchCmd) runDispatch(
	ctx context.Context, info workspace.Info, planPath string,
) error {
	// 1. Validate plan.
	res, exitCode := runValidatePlan(planPath)
	if exitCode != 0 {
		for _, e := range res.Errors {
			fmt.Fprintln(os.Stderr, e)
		}
		return fmt.Errorf("dispatch: plan %q has %d validation error(s)", planPath, len(res.Errors))
	}
	if c.DryRun {
		if !c.Quiet {
			fmt.Printf("dispatch: --dry-run: %q valid (%d slot(s)); no tasks or manifest written\n",
				planPath, len(res.Slots))
		}
		return nil
	}

	// 2. Guard against a conflicting in-flight dispatch.
	if err := guardExistingDispatch(info.WorkspaceDir, planPath); err != nil {
		return err
	}

	// 3-5. Create tasks and write manifest.
	m, taskIDs, err := createTasksAndManifest(ctx, info, planPath, res.Slots)
	if err != nil {
		return err
	}

	// 6. Output summary.
	return c.printDispatchResult(info.WorkspaceDir, planPath, m, taskIDs)
}

// guardExistingDispatch returns an error when a manifest with a different plan
// SHA already exists, preventing accidental cross-plan clobbers.
func guardExistingDispatch(workspaceDir, planPath string) error {
	existing, err := dispatch.Read(workspaceDir)
	if errors.Is(err, dispatch.ErrNoManifest) {
		return nil // no manifest — safe to proceed
	}
	if err != nil {
		return fmt.Errorf("dispatch: read existing manifest: %w", err)
	}
	sha, shaErr := planSHA256(planPath)
	if shaErr != nil {
		return fmt.Errorf("dispatch: compute plan sha: %w", shaErr)
	}
	if existing.PlanSHA256 != sha {
		return fmt.Errorf(
			"dispatch: in-flight dispatch exists (manifest at %s); "+
				"run `--cancel` first or `--advance` to promote the wave",
			dispatch.Path(workspaceDir),
		)
	}
	return nil // same plan — idempotent re-emit
}

// createTasksAndManifest creates one bones-task per slot and writes the manifest.
func createTasksAndManifest(
	ctx context.Context,
	info workspace.Info,
	planPath string,
	slots []slotEntry,
) (dispatch.Manifest, map[string]string, error) {
	mgr, closeMgr, err := openManager(ctx, info)
	if err != nil {
		return dispatch.Manifest{}, nil, fmt.Errorf("dispatch: open manager: %w", err)
	}
	defer closeMgr()
	defer func() { _ = mgr.Close() }()

	taskIDs := make(map[string]string, len(slots))
	now := time.Now().UTC()
	for _, s := range slots {
		id := uuid.NewString()
		t := tasks.Task{
			ID:            id,
			Title:         s.Name,
			Status:        tasks.StatusOpen,
			Files:         []string{},
			Context:       map[string]string{"slot": s.Name},
			CreatedAt:     now,
			UpdatedAt:     now,
			SchemaVersion: tasks.SchemaVersion,
		}
		if err := mgr.Tx(ctx, t.ID, func(tx *tasks.Tx) error {
			return tx.Create(t)
		}); err != nil {
			return dispatch.Manifest{}, nil,
				fmt.Errorf("dispatch: create task for slot %q: %w", s.Name, err)
		}
		taskIDs[s.Name] = id
	}

	m, err := dispatch.BuildManifest(dispatch.BuildOptions{PlanPath: planPath, TaskIDs: taskIDs})
	if err != nil {
		return dispatch.Manifest{}, nil, fmt.Errorf("dispatch: build manifest: %w", err)
	}
	if err := dispatch.Write(info.WorkspaceDir, m); err != nil {
		return dispatch.Manifest{}, nil, fmt.Errorf("dispatch: write manifest: %w", err)
	}
	return m, taskIDs, nil
}

// printDispatchResult prints the dispatch summary to stdout (or JSON when --json).
func (c *SwarmDispatchCmd) printDispatchResult(
	workspaceDir, planPath string,
	m dispatch.Manifest,
	taskIDs map[string]string,
) error {
	manifestPath := dispatch.Path(workspaceDir)
	if c.JSON {
		return emitEnvelope(os.Stdout, "swarm.dispatch",
			schemas.SwarmDispatchPayload{
				ManifestPath: manifestPath,
				TaskCount:    len(taskIDs),
				PlanSHA256:   m.PlanSHA256,
			})
	}
	if c.Quiet {
		return nil
	}
	// Multi-target convention: one line per task, plus a one-line
	// summary, plus the manifest/next-step lines that aren't a per-
	// entity success signature but are still operationally useful.
	// Iterate slots in stable order so the per-task lines are
	// reproducible across runs.
	slotNames := make([]string, 0, len(taskIDs))
	for slot := range taskIDs {
		slotNames = append(slotNames, slot)
	}
	sortStrings(slotNames)
	for _, slot := range slotNames {
		uxprint.Created(os.Stdout, truncateID(taskIDs[slot], 8), slot)
	}
	uxprint.Summary(os.Stdout, len(taskIDs), pluralize("task", len(taskIDs)), "created")
	fmt.Printf("dispatch: manifest written to %s\n", manifestPath)
	fmt.Println("dispatch: next step — run `bones swarm dispatch --advance` after wave 1 completes")
	return nil
}

// sortStrings is a small wrapper around sort.Strings kept local to
// swarm_dispatch.go so the import surface of the file does not grow
// for a one-line operation. The wider sort already lives in the
// package via tasks_slot.go's groupBySlot.
func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j-1] > in[j]; j-- {
			in[j-1], in[j] = in[j], in[j-1]
		}
	}
}

// pluralize returns "task" or "tasks" depending on count, matching
// the brief's "2 tasks created" / "1 task created" examples.
func pluralize(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// runAdvance opens the task manager, wires an isClosed shim, calls
// dispatch.Advance, and prints the promoted wave number.
func (c *SwarmDispatchCmd) runAdvance(ctx context.Context, info workspace.Info) error {
	mgr, closeMgr, err := openManager(ctx, info)
	if err != nil {
		return fmt.Errorf("dispatch advance: open manager: %w", err)
	}
	defer closeMgr()
	defer func() { _ = mgr.Close() }()

	isClosed := func(taskID string) (bool, error) {
		t, _, err := mgr.Get(ctx, taskID)
		if err != nil {
			return false, err
		}
		return t.Status == tasks.StatusClosed, nil
	}

	updated, err := dispatch.Advance(info.WorkspaceDir, isClosed)
	if err != nil {
		if errors.Is(err, dispatch.ErrWaveIncomplete) {
			fmt.Fprintf(os.Stderr, "dispatch: %v\n", err)
			return fmt.Errorf("dispatch advance: wave not yet complete")
		}
		if errors.Is(err, dispatch.ErrAllWavesComplete) {
			if !c.Quiet {
				fmt.Println("dispatch: all waves complete — dispatch is finished")
			}
			return nil
		}
		return err
	}
	if !c.Quiet {
		fmt.Printf("dispatch: advanced to wave %d of %d\n", updated.CurrentWave, len(updated.Waves))
	}
	return nil
}

// runCancel opens the task manager, wires a closeTask shim that uses
// mgr.Update to set each task to Closed with the cancel reason,
// calls dispatch.Cancel, and prints a confirmation.
func (c *SwarmDispatchCmd) runCancel(ctx context.Context, info workspace.Info) error {
	mgr, closeMgr, err := openManager(ctx, info)
	if err != nil {
		return fmt.Errorf("dispatch cancel: open manager: %w", err)
	}
	defer closeMgr()
	defer func() { _ = mgr.Close() }()

	closeTask := func(taskID, reason string) error {
		mutate := func(t tasks.Task) (tasks.Task, error) {
			now := time.Now().UTC()
			t.Status = tasks.StatusClosed
			t.ClosedAt = &now
			if t.ClaimedBy != "" {
				t.ClosedBy = t.ClaimedBy
			}
			t.ClaimedBy = ""
			t.ClosedReason = reason
			t.UpdatedAt = now
			return t, nil
		}
		return mgr.Tx(ctx, taskID, func(tx *tasks.Tx) error {
			return tx.Close("", reason, mutate)
		})
	}

	if err := dispatch.Cancel(info.WorkspaceDir, closeTask); err != nil {
		return err
	}
	if !c.Quiet {
		fmt.Println("dispatch: canceled and manifest removed")
	}
	return nil
}
