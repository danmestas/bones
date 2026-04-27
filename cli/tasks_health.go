package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/coord"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

// TasksStaleCmd lists open/claimed tasks not updated within --days.
type TasksStaleCmd struct {
	Days int  `name:"days" default:"7" help:"minimum age in days"`
	JSON bool `name:"json" help:"emit JSON"`
}

// TasksOrphansCmd lists claimed tasks whose claimer is not currently online.
type TasksOrphansCmd struct {
	JSON bool `name:"json" help:"emit JSON"`
}

// TasksPreflightCmd runs both stale and orphans, returning a combined report.
type TasksPreflightCmd struct {
	Days int  `name:"days" default:"7" help:"minimum stale age in days"`
	JSON bool `name:"json" help:"emit JSON"`
}

type preflightResult struct {
	Stale   []tasks.Task `json:"stale"`
	Orphans []tasks.Task `json:"orphans"`
}

func (c *TasksStaleCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "stale", func(ctx context.Context) error {
		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()
		allTasks, err := mgr.List(ctx)
		if err != nil {
			return err
		}
		cutoff := time.Now().UTC().Add(-time.Duration(c.Days) * 24 * time.Hour)
		stale := findStaleTasks(allTasks, cutoff)
		if c.JSON {
			return emitJSON(os.Stdout, stale)
		}
		for _, t := range stale {
			fmt.Println(formatListLine(t))
		}
		return nil
	}))
}

func (c *TasksOrphansCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "orphans", func(ctx context.Context) error {
		orphans, err := loadOrphans(ctx, info)
		if err != nil {
			return err
		}
		if c.JSON {
			return emitJSON(os.Stdout, orphans)
		}
		for _, t := range orphans {
			fmt.Println(formatListLine(t))
		}
		return nil
	}))
}

func (c *TasksPreflightCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "preflight", func(ctx context.Context) error {
		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()
		allTasks, err := mgr.List(ctx)
		if err != nil {
			return err
		}
		cutoff := time.Now().UTC().Add(-time.Duration(c.Days) * 24 * time.Hour)
		result := preflightResult{Stale: findStaleTasks(allTasks, cutoff)}
		result.Orphans, err = loadOrphans(ctx, info)
		if err != nil {
			return err
		}
		if c.JSON {
			return emitJSON(os.Stdout, result)
		}
		for _, t := range result.Stale {
			fmt.Printf("stale %s\n", formatListLine(t))
		}
		for _, t := range result.Orphans {
			fmt.Printf("orphan %s\n", formatListLine(t))
		}
		return nil
	}))
}

func loadOrphans(ctx context.Context, info workspace.Info) ([]tasks.Task, error) {
	mgr, closeNC, err := openManager(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("open manager: %w", err)
	}
	defer closeNC()
	defer func() { _ = mgr.Close() }()
	allTasks, err := mgr.List(ctx)
	if err != nil {
		return nil, err
	}
	coordSession, err := coord.Open(ctx, newCoordConfig(info))
	if err != nil {
		return nil, fmt.Errorf("open coord: %w", err)
	}
	defer func() { _ = coordSession.Close() }()
	peers, err := coordSession.Who(ctx)
	if err != nil {
		return nil, err
	}
	return findOrphanTasks(allTasks, liveAgentSet(peers)), nil
}

func liveAgentSet(peers []coord.Presence) map[string]struct{} {
	out := make(map[string]struct{}, len(peers))
	for _, p := range peers {
		out[p.AgentID()] = struct{}{}
	}
	return out
}

func findStaleTasks(allTasks []tasks.Task, cutoff time.Time) []tasks.Task {
	out := make([]tasks.Task, 0, len(allTasks))
	for _, t := range allTasks {
		if t.Status == tasks.StatusClosed {
			continue
		}
		if t.UpdatedAt.After(cutoff) {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.Before(out[j].UpdatedAt)
	})
	return out
}

func findOrphanTasks(
	allTasks []tasks.Task,
	liveAgents map[string]struct{},
) []tasks.Task {
	out := make([]tasks.Task, 0, len(allTasks))
	for _, t := range allTasks {
		if t.Status != tasks.StatusClaimed || t.ClaimedBy == "" {
			continue
		}
		if _, ok := liveAgents[t.ClaimedBy]; ok {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.Before(out[j].UpdatedAt)
	})
	return out
}
