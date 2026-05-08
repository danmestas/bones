package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/cli/schemas"
	"github.com/danmestas/bones/cli/uxprint"
	"github.com/danmestas/bones/internal/tasks"
)

// TasksAggregateCmd produces a per-slot summary of tasks within a window.
type TasksAggregateCmd struct {
	Since time.Duration `name:"since" default:"1h" help:"window size"`
	JSON  bool          `name:"json" help:"emit JSON"`
}

// aggregateSlot holds the per-slot summary computed by the aggregate
// verb. JSON tags mirror the schemas.TasksAggregateSlot wire shape;
// the fields are name-equal so the struct round-trips cleanly into
// the envelope payload below.
type aggregateSlot struct {
	SlotID string   `json:"slot_id"`
	Tasks  int      `json:"tasks"`
	Files  []string `json:"files"`
	Status string   `json:"status"`
}

func (c *TasksAggregateCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "aggregate", func(ctx context.Context) error {
		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()

		allTasks, err := mgr.List(ctx)
		if err != nil {
			return fmt.Errorf("aggregate: list tasks: %w", err)
		}
		slots, totalTasks, activeSlots := buildAggregateSlots(allTasks, c.Since)
		// hasOlder distinguishes "your --since hid these rows"
		// (filter-emptiness hint shown) from "the workspace has no
		// task history at all" (silent). The hint exists to point
		// the operator at the escape hatch (--since=) when their
		// window is the reason for emptiness.
		hasOlder := totalTasks == 0 && len(allTasks) > 0
		return emitAggregateOutput(c.Since, slots, totalTasks, activeSlots, c.JSON, hasOlder)
	}))
}

// buildAggregateSlots buckets tasks by claiming agent for the given window.
func buildAggregateSlots(
	allTasks []tasks.Task, since time.Duration,
) ([]aggregateSlot, int, int) {
	cutoff := time.Now().UTC().Add(-since)
	slotMap := map[string]*aggregateSlot{}
	total := 0
	for _, t := range allTasks {
		agent := resolvedAgent(t)
		if agent == "" {
			continue
		}
		if t.CreatedAt.Before(cutoff) && t.UpdatedAt.Before(cutoff) {
			continue
		}
		total++
		s, ok := slotMap[agent]
		if !ok {
			s = &aggregateSlot{SlotID: agent, Status: "closed"}
			slotMap[agent] = s
		}
		s.Tasks++
		s.Files = appendUniq(s.Files, t.Files...)
		if t.Status == tasks.StatusClaimed {
			s.Status = "active"
		}
	}
	slots := make([]aggregateSlot, 0, len(slotMap))
	for _, s := range slotMap {
		sort.Strings(s.Files)
		slots = append(slots, *s)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].SlotID < slots[j].SlotID })
	activeSlots := 0
	for _, s := range slots {
		if s.Status == "active" {
			activeSlots++
		}
	}
	return slots, total, activeSlots
}

func emitAggregateOutput(
	since time.Duration,
	slots []aggregateSlot,
	totalTasks, activeSlots int,
	asJSON, hasOlder bool,
) error {
	if asJSON {
		schemaSlots := make([]schemas.TasksAggregateSlot, len(slots))
		for i, s := range slots {
			schemaSlots[i] = schemas.TasksAggregateSlot{
				SlotID: s.SlotID,
				Tasks:  s.Tasks,
				Files:  s.Files,
				Status: s.Status,
			}
		}
		payload := schemas.TasksAggregatePayload{
			Since:       since.String(),
			TotalTasks:  totalTasks,
			TotalSlots:  len(slots),
			ActiveSlots: activeSlots,
			Slots:       schemaSlots,
		}
		return emitEnvelope(os.Stdout, "tasks.aggregate", payload)
	}
	return printAggregateSummary(since, slots, totalTasks, activeSlots, hasOlder)
}

func printAggregateSummary(
	since time.Duration,
	slots []aggregateSlot,
	totalTasks, activeSlots int,
	hasOlder bool,
) error {
	sep := strings.Repeat("─", 53)
	var b strings.Builder
	fmt.Fprintf(&b, "Run summary (last %s)\n", since)
	fmt.Fprintln(&b, sep)
	if len(slots) == 0 {
		// Filter-emptiness hint: if there are older tasks but the
		// --since window hid them, point the operator at the escape
		// hatch. When the workspace has no tasks at all, fall back
		// to the legacy "(no tasks in window)" line so the report
		// frame is still readable.
		if hasOlder {
			uxprint.NoRecentActivity(&b, since.String())
		} else {
			fmt.Fprintln(&b, "(no tasks in window)")
		}
	}
	for _, s := range slots {
		summary := summarizeFiles(s.Files, 3)
		fmt.Fprintf(&b, "%-20s  %2d task(s)  files: %-30s  status: %s\n",
			s.SlotID, s.Tasks, summary, s.Status)
	}
	fmt.Fprintln(&b, sep)
	fmt.Fprintf(&b, "%d task(s) total · %d slot(s) · %d active\n",
		totalTasks, len(slots), activeSlots)
	_, err := fmt.Fprint(os.Stdout, b.String())
	return err
}

func resolvedAgent(t tasks.Task) string {
	if t.ClosedBy != "" {
		return t.ClosedBy
	}
	return t.ClaimedBy
}

func appendUniq(dst []string, src ...string) []string {
	seen := make(map[string]struct{}, len(dst))
	for _, s := range dst {
		seen[s] = struct{}{}
	}
	for _, s := range src {
		if _, ok := seen[s]; !ok {
			dst = append(dst, s)
			seen[s] = struct{}{}
		}
	}
	return dst
}

func summarizeFiles(files []string, max int) string {
	if len(files) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		base := f
		if idx := strings.LastIndex(f, "/"); idx >= 0 {
			base = f[idx+1:]
		}
		names = append(names, base)
	}
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + fmt.Sprintf(", +%d more", len(names)-max)
}

// Note: the parseAggregateFlags helper from cmd/agent-tasks/aggregate.go is
// no longer needed — Kong parses --since and --json directly into the
// TasksAggregateCmd struct.
