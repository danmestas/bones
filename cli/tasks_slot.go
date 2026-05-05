package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/danmestas/bones/internal/tasks"
)

// HotSlotThreshold is the open-task count above which a slot is
// considered "hot" — packed past the point of plausibly-deliberate
// authoring. The substrate runs exactly one `coord.Leaf` per slot at a
// time (ADR 0023 architectural invariant 5; ADR 0028 architectural
// invariant 1: "Per-slot leaf process — each active slot owns exactly
// one `coord.Leaf`"), so N tasks in one slot run serially regardless of
// file-disjointness or task-graph independence. Issue #214's heuristic:
// more than 4 open tasks in a single slot is almost certainly a
// packing mistake, because parallelism in bones comes from N distinct
// slots, not from depth within one slot. Kept as a single named
// constant (not a flag, not workspace-configurable) per the issue's
// "avoid per-workspace tuning becoming a foot-gun" guidance.
const HotSlotThreshold = 4

// unslottedSlotKey is the synthetic bucket label used when grouping
// tasks that lack a Context["slot"] annotation. The leading "(" makes
// it sort before any real slot name so plan authors see un-annotated
// work first; ADR 0023's plan validator forbids slot-less tasks at
// dispatch time, but the inspection view is run at plan-authoring time
// where tasks may legitimately not yet carry a slot.
const unslottedSlotKey = "(unslotted)"

// slotGroup is one row of the by-slot inspection view. JSON tags
// match the documented harness-consumable shape so callers can parse
// the same payload regardless of whether `--by-slot` was rendered for
// a human or a tool.
type slotGroup struct {
	// Slot is the Context["slot"] value, or unslottedSlotKey for tasks
	// without a slot annotation.
	Slot string `json:"slot"`

	// OpenCount is the number of non-closed (open or claimed) tasks in
	// this slot. Closed tasks free the slot for the next leaf — they
	// must not count against serialization depth.
	OpenCount int `json:"open_count"`

	// Hot is true when OpenCount exceeds HotSlotThreshold. The boolean
	// is the harness-consumable form of the textual indicator the
	// plain-text view renders ("HOT").
	Hot bool `json:"hot"`

	// TaskIDs lists the IDs of the non-closed tasks in this slot, in
	// the order they appeared in the source list. Useful for harnesses
	// that want to drill down without a second `tasks list` call.
	TaskIDs []string `json:"task_ids"`
}

// groupBySlot buckets tasks by their Context["slot"] value, counting
// non-closed tasks per slot. Closed tasks are dropped — they do not
// occupy a leaf. Slot ordering is alphabetical, with the synthetic
// unslotted bucket sorting first by virtue of its leading "(".
func groupBySlot(in []tasks.Task) []slotGroup {
	buckets := map[string]*slotGroup{}
	for _, t := range in {
		if t.Status == tasks.StatusClosed {
			continue
		}
		key := t.Context["slot"]
		if key == "" {
			key = unslottedSlotKey
		}
		g, ok := buckets[key]
		if !ok {
			g = &slotGroup{Slot: key}
			buckets[key] = g
		}
		g.OpenCount++
		g.TaskIDs = append(g.TaskIDs, t.ID)
	}
	out := make([]slotGroup, 0, len(buckets))
	for _, g := range buckets {
		g.Hot = g.OpenCount > HotSlotThreshold
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

// emitBySlot renders the slot-grouped view, either as JSON for harness
// consumption or as a plain-text table that names the serialization
// rule explicitly so an operator reading the warning does not have to
// know ADR 0023/0028 to act on it.
func emitBySlot(w io.Writer, groups []slotGroup, asJSON bool) error {
	if asJSON {
		payload := struct {
			Slots     []slotGroup `json:"slots"`
			Threshold int         `json:"hot_threshold"`
		}{
			Slots:     groups,
			Threshold: HotSlotThreshold,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal by-slot: %w", err)
		}
		data = append(data, '\n')
		_, err = w.Write(data)
		return err
	}
	// Plain-text rendering. The caption is the serialization rule
	// surfaced inline — operators must not have to read ADRs to
	// understand why the count matters.
	if _, err := fmt.Fprintf(w,
		"bones runs one leaf per slot at a time; tasks sharing a slot "+
			"serialize. Slots with >%d open tasks are flagged HOT — "+
			"likely a planner packing mistake.\n",
		HotSlotThreshold); err != nil {
		return err
	}
	if len(groups) == 0 {
		_, err := fmt.Fprintln(w, "(no open tasks)")
		return err
	}
	for _, g := range groups {
		marker := "    "
		if g.Hot {
			marker = "HOT "
		}
		if _, err := fmt.Fprintf(w, "%s%-24s %3d open\n",
			marker, g.Slot, g.OpenCount); err != nil {
			return err
		}
	}
	return nil
}

// countOpenInSlot returns the number of non-closed tasks whose
// Context["slot"] equals slot. Empty slot is a no-op (returns 0) — the
// hot-slot warning is opt-in via --slot at create time. Used by both
// `tasks create`'s post-mutation warning (issue #214 fix C) and any
// other caller that needs the depth without re-deriving from a list.
func countOpenInSlot(in []tasks.Task, slot string) int {
	if slot == "" {
		return 0
	}
	n := 0
	for _, t := range in {
		if t.Status == tasks.StatusClosed {
			continue
		}
		if t.Context["slot"] == slot {
			n++
		}
	}
	return n
}

// maybeWarnHotSlot writes a one-line advisory to w when the
// post-mutation depth crosses HotSlotThreshold. Empty slot is a no-op
// (the feature is slot-scoped). The wording names the slot, the new
// depth, and the consequence so the operator can correlate the warning
// to ADR 0023/0028 without reading the ADRs. Issue #214 explicitly
// requires the warning to be advisory only — the caller does not
// inspect the return value, the mutation must still succeed, and the
// exit code does not change. Re-warning every time growth lands in
// the hot range is intentional: each create that adds to a hot slot
// is a packing mistake to surface, not just the first crossing.
func maybeWarnHotSlot(w io.Writer, slot string, newDepth int) {
	if slot == "" || newDepth <= HotSlotThreshold {
		return
	}
	_, _ = fmt.Fprintf(w,
		"warning: slot %q now has %d open tasks (>%d); "+
			"bones runs one leaf per slot, so these will serialize. "+
			"Spread tasks across distinct slots to recover parallelism. "+
			"Inspect with: bones tasks list --by-slot\n",
		slot, newDepth, HotSlotThreshold)
}
