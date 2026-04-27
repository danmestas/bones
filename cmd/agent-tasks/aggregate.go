package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
	"github.com/danmestas/agent-infra/internal/workspace"
)

func init() {
	handlers["aggregate"] = aggregateCmd
}

// aggregateSlot holds the per-slot summary computed by aggregateCmd.
type aggregateSlot struct {
	// SlotID is the claiming agent's ID.
	SlotID string `json:"slot_id"`
	// Tasks is the number of tasks claimed or closed by this slot.
	Tasks int `json:"tasks"`
	// Files is the union of all files across this slot's tasks.
	Files []string `json:"files"`
	// Status is the slot's aggregate status: "closed" when all tasks are
	// closed, "active" when any task remains claimed.
	Status string `json:"status"`
}

// aggregateResult is the JSON output shape for --json.
type aggregateResult struct {
	Since       string          `json:"since"`
	TotalTasks  int             `json:"total_tasks"`
	TotalSlots  int             `json:"total_slots"`
	ActiveSlots int             `json:"active_slots"`
	Slots       []aggregateSlot `json:"slots"`
}

func aggregateCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "aggregate", func(ctx context.Context) error {
		since, asJSON, err := parseAggregateFlags(args)
		if err != nil {
			return err
		}
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
		slots, totalTasks, activeSlots := buildAggregateSlots(allTasks, since)
		return emitAggregateOutput(since, slots, totalTasks, activeSlots, asJSON)
	})
}

// parseAggregateFlags parses --since and --json from args.
func parseAggregateFlags(args []string) (time.Duration, bool, error) {
	since := time.Hour
	asJSON := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "--since="):
			d, err := time.ParseDuration(strings.TrimPrefix(a, "--since="))
			if err != nil {
				return 0, false, fmt.Errorf("aggregate: --since: %w", err)
			}
			since = d
		case a == "--since" && i+1 < len(args):
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return 0, false, fmt.Errorf("aggregate: --since: %w", err)
			}
			since = d
		default:
			return 0, false, fmt.Errorf(
				"aggregate: unknown flag %q (want --since <duration> --json)", a)
		}
	}
	return since, asJSON, nil
}

// buildAggregateSlots buckets tasks by claiming agent for the given window
// and returns sorted slots, total task count, and active-slot count.
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

// emitAggregateOutput writes the run summary to stdout.
func emitAggregateOutput(
	since time.Duration,
	slots []aggregateSlot,
	totalTasks, activeSlots int,
	asJSON bool,
) error {
	if asJSON {
		res := aggregateResult{
			Since:       since.String(),
			TotalTasks:  totalTasks,
			TotalSlots:  len(slots),
			ActiveSlots: activeSlots,
			Slots:       slots,
		}
		data, err := json.Marshal(res)
		if err != nil {
			return fmt.Errorf("aggregate: marshal: %w", err)
		}
		data = append(data, '\n')
		_, err = os.Stdout.Write(data)
		return err
	}
	return printAggregateSummary(since, slots, totalTasks, activeSlots)
}

// printAggregateSummary writes the human-readable aggregate table to stdout.
func printAggregateSummary(
	since time.Duration,
	slots []aggregateSlot,
	totalTasks, activeSlots int,
) error {
	sep := strings.Repeat("─", 53)
	var b strings.Builder
	fmt.Fprintf(&b, "Run summary (last %s)\n", since)
	fmt.Fprintln(&b, sep)
	if len(slots) == 0 {
		fmt.Fprintln(&b, "(no tasks in window)")
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

// resolvedAgent returns the agent ID most recently responsible for a task:
// ClosedBy for closed tasks, ClaimedBy for claimed tasks, or "" if never
// claimed.
func resolvedAgent(t tasks.Task) string {
	if t.ClosedBy != "" {
		return t.ClosedBy
	}
	return t.ClaimedBy
}

// appendUniq appends elements of src into dst, skipping duplicates.
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

// summarizeFiles returns up to max file basenames joined by ", "; if there
// are more, appends "+N more".
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
