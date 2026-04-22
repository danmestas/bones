package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/tasks"
	"github.com/danmestas/agent-infra/internal/workspace"
)

type preflightResult struct {
	Stale   []tasks.Task `json:"stale"`
	Orphans []tasks.Task `json:"orphans"`
}

func init() {
	handlers["stale"] = staleCmd
	handlers["orphans"] = orphansCmd
	handlers["preflight"] = preflightCmd
}

func staleCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "stale", func(ctx context.Context) error {
		fs := flag.NewFlagSet("stale", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		days := fs.Int("days", 7, "minimum age in days")
		asJSON := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		mgr, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer func() { _ = mgr.Close() }()
		allTasks, err := mgr.List(ctx)
		if err != nil {
			return err
		}
		cutoff := time.Now().UTC().Add(-time.Duration(*days) * 24 * time.Hour)
		stale := findStaleTasks(allTasks, cutoff)
		if *asJSON {
			return emitJSON(os.Stdout, stale)
		}
		for _, t := range stale {
			fmt.Println(formatListLine(t))
		}
		return nil
	})
}

func orphansCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "orphans", func(ctx context.Context) error {
		fs := flag.NewFlagSet("orphans", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		asJSON := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		orphans, err := loadOrphans(ctx, info)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(os.Stdout, orphans)
		}
		for _, t := range orphans {
			fmt.Println(formatListLine(t))
		}
		return nil
	})
}

func preflightCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "preflight", func(ctx context.Context) error {
		fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		days := fs.Int("days", 7, "minimum stale age in days")
		asJSON := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		mgr, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer func() { _ = mgr.Close() }()
		allTasks, err := mgr.List(ctx)
		if err != nil {
			return err
		}
		cutoff := time.Now().UTC().Add(-time.Duration(*days) * 24 * time.Hour)
		result := preflightResult{Stale: findStaleTasks(allTasks, cutoff)}
		result.Orphans, err = loadOrphans(ctx, info)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitJSON(os.Stdout, result)
		}
		for _, t := range result.Stale {
			fmt.Printf("stale %s\n", formatListLine(t))
		}
		for _, t := range result.Orphans {
			fmt.Printf("orphan %s\n", formatListLine(t))
		}
		return nil
	})
}

func loadOrphans(ctx context.Context, info workspace.Info) ([]tasks.Task, error) {
	mgr, err := openManager(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("open manager: %w", err)
	}
	defer func() { _ = mgr.Close() }()
	allTasks, err := mgr.List(ctx)
	if err != nil {
		return nil, err
	}
	c, err := coord.Open(ctx, newCoordConfig(info))
	if err != nil {
		return nil, fmt.Errorf("open coord: %w", err)
	}
	defer func() { _ = c.Close() }()
	peers, err := c.Who(ctx)
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
