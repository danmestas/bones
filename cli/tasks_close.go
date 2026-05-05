package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

// TasksCloseCmd closes a task. When the task is bound to a live swarm
// slot (a session record in bones-swarm-sessions carries this task's
// id), the matching slot is auto-released via the existing
// `bones swarm close` path so the operator does not have to remember a
// separate cleanup step (issue #209, ADR 0049).
//
// --keep-slot opts out of the auto-release for the rare case where the
// operator wants the slot to outlive the task it was claimed for. The
// auto-release path uses the same artifact precondition swarm close
// uses; if it would refuse (no commit since join) then `tasks close`
// itself refuses and leaves the task state unchanged so close is
// atomic across the two layers.
type TasksCloseCmd struct {
	ID       string `arg:"" help:"task id"`
	Reason   string `name:"reason" help:"close reason (optional)"`
	KeepSlot bool   `name:"keep-slot" help:"close the task; leave the swarm slot intact"`
	JSON     bool   `name:"json" help:"emit JSON"`
}

func (c *TasksCloseCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "close", func(ctx context.Context) error {
		return c.runClose(ctx, info)
	}))
}

// runClose dispatches between the auto-release path (slot-leaf claim
// + live session, atomic with the close) and the legacy path
// (manager-only update). Factored out so integration tests can
// drive close behavior without an os.Getwd dance.
func (c *TasksCloseCmd) runClose(ctx context.Context, info workspace.Info) error {
	mgr, closeNC, err := openManager(ctx, info)
	if err != nil {
		return fmt.Errorf("open manager: %w", err)
	}
	defer closeNC()
	defer func() { _ = mgr.Close() }()

	// Read the task before deciding which path to take so we can
	// inspect ClaimedBy without racing the mutate closure inside
	// Update. A concurrent re-claim between this Get and the close
	// is rare; if it happens, the legacy Update path's CAS loop
	// catches it and the auto-release path re-validates against
	// the session record (which is also CAS-gated).
	cur, _, err := mgr.Get(ctx, c.ID)
	if err != nil {
		return err
	}

	if !c.KeepSlot {
		slot, ok, lookupErr := lookupSlotForTask(ctx, info, c.ID, cur.ClaimedBy)
		if lookupErr != nil {
			return lookupErr
		}
		if ok {
			return c.autoReleaseAndClose(ctx, info, slot)
		}
	}
	return c.legacyClose(ctx, mgr, info)
}

// legacyClose is today's pre-#209 path: the tasks Manager Update
// closure stamps Status=Closed plus the close attribution fields and
// CAS-writes them. Used when --keep-slot is set or when ClaimedBy
// does not identify a live swarm slot.
func (c *TasksCloseCmd) legacyClose(
	ctx context.Context, mgr *tasks.Manager, info workspace.Info,
) error {
	var updated tasks.Task
	err := mgr.Update(ctx, c.ID, func(t tasks.Task) (tasks.Task, error) {
		now := time.Now().UTC()
		t.Status = tasks.StatusClosed
		t.ClosedAt = &now
		// ClosedBy attributes the work, not the click. If the task
		// was claimed, the claimer did the work — even if the
		// workspace agent (or an admin) is what physically ran
		// `bones tasks close`. Without this, invariant 11 (claimed_by
		// empty when not claimed) erases the original claimer and
		// every closed-task aggregate buckets under the workspace
		// UUID. Coord-level close already attributes to the caller
		// (which equals the claimer there, since it enforces a
		// claim-match precondition), so this aligns the two paths.
		if t.ClaimedBy != "" {
			t.ClosedBy = t.ClaimedBy
		} else {
			t.ClosedBy = info.AgentID
		}
		t.ClosedReason = c.Reason
		t.UpdatedAt = now
		// Invariant 11: claimed_by must be empty when status != claimed.
		t.ClaimedBy = ""
		updated = t
		return t, nil
	})
	if err != nil {
		return err
	}
	if c.JSON {
		return emitJSON(os.Stdout, updated)
	}
	return nil
}

// autoReleaseAndClose runs the swarm-close path on the matched slot,
// closing the task in KV and deleting the session record in one
// CAS-gated operation (per ADR 0028's success-close contract). When
// the underlying swarm.ResumedLease.Close refuses (artifact
// precondition: no commit since join) the entire tasks close
// returns the refusal — task state is NOT updated, the session
// record is NOT removed, and the error names the slot so the
// operator knows where to commit or where to pass --keep-slot.
func (c *TasksCloseCmd) autoReleaseAndClose(
	ctx context.Context, info workspace.Info, slot string,
) error {
	hubURL := resolveHubURL("")
	lease, err := swarm.Resume(ctx, info, slot, swarm.AcquireOpts{HubURL: hubURL})
	if err != nil {
		// Session evaporated between detection and Resume. Fall
		// back to legacy: there is no slot to release any more so
		// the task close should still proceed. lookupSlotForClaim
		// already filtered ErrNotFound; this branch covers the
		// narrow race where the record was deleted in between.
		if errors.Is(err, swarm.ErrSessionNotFound) {
			return c.runClose(ctx, info)
		}
		return fmt.Errorf("tasks close: resume slot %q: %w", slot, err)
	}
	closeErr := lease.Close(ctx, swarm.CloseOpts{CloseTaskOnSuccess: true})
	if closeErr == nil {
		fmt.Fprintf(os.Stderr,
			"tasks close: closed task %s and released slot %s\n", c.ID, slot)
		if c.JSON {
			// Re-read the task so the JSON shape includes the close
			// fields the lease/coord layer just wrote.
			updated, err := readTaskAfterClose(ctx, info, c.ID)
			if err != nil {
				return err
			}
			return emitJSON(os.Stdout, updated)
		}
		return nil
	}
	if errors.Is(closeErr, swarm.ErrCloseRequiresArtifact) {
		return fmt.Errorf(
			"tasks close: refusing — slot %q has no commit since join "+
				"(swarm-close artifact precondition); commit pending work "+
				"with `bones swarm commit -m ...` then retry, or pass "+
				"--keep-slot to close the task and leave the slot intact",
			slot,
		)
	}
	return fmt.Errorf("tasks close: swarm close slot %q: %w", slot, closeErr)
}

// readTaskAfterClose re-fetches the task record after a swarm close
// has mutated it via coord.Leaf.Close → coord.CloseTask. Pulled out
// so the JSON-emit branch reuses the standard manager-Open shape.
func readTaskAfterClose(
	ctx context.Context, info workspace.Info, id string,
) (tasks.Task, error) {
	mgr, closeNC, err := openManager(ctx, info)
	if err != nil {
		return tasks.Task{}, fmt.Errorf("read after close: %w", err)
	}
	defer closeNC()
	defer func() { _ = mgr.Close() }()
	t, _, err := mgr.Get(ctx, id)
	if err != nil {
		return tasks.Task{}, fmt.Errorf("read after close: %w", err)
	}
	return t, nil
}

// lookupSlotForTask returns the slot whose live swarm session record
// is bound to taskID. The auto-release path triggers on this match:
// a session record carrying this task's id is the structural binding
// between the task and the slot's leaf — it survives the inter-verb
// windows where ClaimedBy is empty (a re-claim/release cycle inside
// every swarm verb un-claims the task at verb exit).
//
// claimedBy is consulted as a tiebreaker only: when the task's
// claim string is set and ends in `-leaf`, the slot embedded in
// the claim is preferred over a session-table scan. This handles
// the rare in-flight-verb window where ClaimedBy is set AND the
// session table has multiple entries (a brand-new dispatch racing
// the close).
//
// Returns ("", false, nil) when no live slot owns this task (the
// expected case for tasks claimed via `bones tasks claim`, or
// closed without ever joining swarm). Returns a non-nil error only
// on substrate failure.
func lookupSlotForTask(
	ctx context.Context, info workspace.Info, taskID, claimedBy string,
) (string, bool, error) {
	// Primary detection: session record's task_id == taskID.
	nc, err := nats.Connect(info.NATSURL)
	if err != nil {
		return "", false, fmt.Errorf("tasks close: nats connect: %w", err)
	}
	defer nc.Close()
	sess, err := swarm.Open(ctx, swarm.Config{NATSConn: nc})
	if err != nil {
		return "", false, fmt.Errorf("tasks close: swarm.Open: %w", err)
	}
	defer func() { _ = sess.Close() }()

	// Tiebreaker: if claimedBy ends in `-leaf`, prefer that slot
	// when its session record exists and matches the task. The
	// session-record check still gates so a stale claim string
	// against a torn-down slot does not trigger auto-release.
	if hint := slotFromClaim(claimedBy); hint != "" {
		s, _, getErr := sess.Get(ctx, hint)
		if getErr == nil && s.TaskID == taskID {
			return hint, true, nil
		}
		if getErr != nil && !errors.Is(getErr, swarm.ErrNotFound) {
			return "", false, fmt.Errorf("tasks close: read session %q: %w", hint, getErr)
		}
	}

	all, err := sess.List(ctx)
	if err != nil {
		return "", false, fmt.Errorf("tasks close: list sessions: %w", err)
	}
	for _, s := range all {
		if s.TaskID == taskID {
			return s.Slot, true, nil
		}
	}
	return "", false, nil
}

// slotFromClaim extracts the slot name from a `<slot>-leaf` claim
// string. Returns "" when claimedBy is empty or does not match the
// slot-leaf shape (e.g. an operator-claim string set by `bones
// tasks claim`).
func slotFromClaim(claimedBy string) string {
	if claimedBy == "" || !strings.HasSuffix(claimedBy, "-leaf") {
		return ""
	}
	return strings.TrimSuffix(claimedBy, "-leaf")
}
