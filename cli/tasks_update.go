package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/tasks"
)

// TasksUpdateCmd updates a task. Flags are pointer-typed so we can detect
// "flag absent" vs "flag set to empty string" — a distinction the
// underlying mutator depends on (only set fields get applied).
type TasksUpdateCmd struct {
	ID         string   `arg:"" help:"task id"`
	Status     string   `name:"status" help:"open|claimed|closed"`
	Title      *string  `name:"title" help:"new title"`
	Files      *string  `name:"files" help:"comma-separated file list (replaces existing)"`
	Parent     *string  `name:"parent" help:"parent task id"`
	DeferUntil *string  `name:"defer-until" help:"RFC3339 time (empty clears)"`
	Context    []string `name:"context" help:"key=value (repeatable; merges)" sep:"none"`
	ClaimedBy  *string  `name:"claimed-by" help:"agent id to claim as"`
	JSON       bool     `name:"json" help:"emit JSON"`
}

func (c *TasksUpdateCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "update", func(ctx context.Context) error {
		if err := validateContextPairs(c.Context); err != nil {
			return err
		}

		var statusUpdate tasks.Status
		if c.Status != "" {
			s, err := parseStatus(c.Status)
			if err != nil {
				return err
			}
			statusUpdate = s
		}

		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()

		var deferUntilStr string
		if c.DeferUntil != nil {
			deferUntilStr = *c.DeferUntil
		}
		parsedDeferUntil, err := parseRFC3339Flag("defer-until", deferUntilStr)
		if err != nil {
			return err
		}

		// Pre-read so we can author real (field, old, new) tuples for
		// the updated event payload. The CAS retry loop inside Tx
		// handles racing writers; the tuples we emit reflect the
		// pre-image we observed plus the mutator's intent.
		before, _, err := mgr.Get(ctx, c.ID)
		if err != nil {
			return err
		}
		var updated tasks.Task
		mutate := buildUpdateMutator(c, statusUpdate, parsedDeferUntil, &updated)
		// Run the mutator against a copy to derive the post-image for
		// the event payload. The same closure runs again inside Tx's
		// CAS loop; both invocations are idempotent.
		afterPreview, mErr := mutate(before)
		if mErr != nil {
			return mErr
		}
		changes := diffTaskFields(before, afterPreview)
		if len(changes) == 0 {
			// Nothing changed — short-circuit so we do not emit an
			// empty event. Idempotent on the CLI side.
			updated = before
			if c.JSON {
				return emitJSON(os.Stdout, updated)
			}
			return nil
		}
		if err := mgr.Tx(ctx, c.ID, func(tx *tasks.Tx) error {
			return tx.Update(mutate, changes...)
		}); err != nil {
			return err
		}
		if c.JSON {
			return emitJSON(os.Stdout, updated)
		}
		return nil
	}))
}

// diffTaskFields returns the FieldChange tuples describing the
// observable differences between before and after. Used by the CLI
// update path to author a real updated-event payload — recovery's
// per-field replay (ADR 0052 §"Recovery") consumes these tuples to
// reconstruct the projection from the log alone.
//
// Compaction-bookkeeping fields (UpdatedAt, SchemaVersion,
// LastEventSeq, the Compact* trio) are deliberately excluded: they
// are not user-driven mutations and would clutter the audit trail.
func diffTaskFields(before, after tasks.Task) []tasks.FieldChange {
	out := make([]tasks.FieldChange, 0, 8)
	if before.Title != after.Title {
		out = append(out, tasks.MustFieldChange("title", before.Title, after.Title))
	}
	if before.Status != after.Status {
		out = append(out, tasks.MustFieldChange("status", before.Status, after.Status))
	}
	if before.ClaimedBy != after.ClaimedBy {
		out = append(out,
			tasks.MustFieldChange("claimed_by", before.ClaimedBy, after.ClaimedBy))
	}
	if before.Parent != after.Parent {
		out = append(out, tasks.MustFieldChange("parent", before.Parent, after.Parent))
	}
	if !filesEqual(before.Files, after.Files) {
		out = append(out, tasks.MustFieldChange("files", before.Files, after.Files))
	}
	if !contextEqual(before.Context, after.Context) {
		out = append(out, tasks.MustFieldChange("context", before.Context, after.Context))
	}
	if !deferUntilEqual(before.DeferUntil, after.DeferUntil) {
		out = append(out,
			tasks.MustFieldChange("defer_until", before.DeferUntil, after.DeferUntil))
	}
	return out
}

func filesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contextEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func deferUntilEqual(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}

// buildUpdateMutator returns the closure passed to Manager.Update, applying
// only the flags explicitly set. Includes invariant-11 status coupling when
// --claimed-by is set without --status.
func buildUpdateMutator(
	c *TasksUpdateCmd,
	statusUpdate tasks.Status,
	deferUntil *time.Time,
	out *tasks.Task,
) func(tasks.Task) (tasks.Task, error) {
	titleSet := c.Title != nil
	filesSet := c.Files != nil
	parentSet := c.Parent != nil
	deferUntilSet := c.DeferUntil != nil
	claimedBySet := c.ClaimedBy != nil

	return func(t tasks.Task) (tasks.Task, error) {
		if statusUpdate != "" {
			t.Status = statusUpdate
		}
		if titleSet {
			t.Title = *c.Title
		}
		if filesSet {
			t.Files = splitFiles(*c.Files)
		}
		if parentSet {
			t.Parent = *c.Parent
		}
		if deferUntilSet {
			t.DeferUntil = deferUntil
		}
		if claimedBySet {
			t.ClaimedBy = *c.ClaimedBy
			// Invariant 11 couples claimed_by to status: non-empty iff
			// status == claimed. If the user set --claimed-by without
			// also setting --status, infer the status from the value.
			if statusUpdate == "" {
				if *c.ClaimedBy != "" {
					t.Status = tasks.StatusClaimed
				} else if t.Status == tasks.StatusClaimed {
					t.Status = tasks.StatusOpen
				}
			}
		}
		t.Context = applyContext(t.Context, c.Context)
		t.UpdatedAt = time.Now().UTC()
		*out = t
		return t, nil
	}
}
