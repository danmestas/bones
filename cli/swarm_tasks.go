package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

// SwarmTasksCmd lists ready tasks scoped to a slot. Wraps the same
// readiness model as `bones tasks list --ready` (coord.Ready handles
// blocks/supersedes/duplicates/parent edges) and filters the result
// to tasks whose slot annotation matches --slot.
//
// Slot membership is resolved in three places, first-match-wins:
//
//  1. Task.Context["slot"] equals --slot
//  2. Task title contains "[slot: <name>]" literal
//  3. Any file path starts with "<slot>/" or contains "/<slot>/"
//
// (3) mirrors the validate_plan slot-disjointness rule so plans that
// don't yet stamp Context["slot"] still surface their tasks here.
type SwarmTasksCmd struct {
	Slot string `name:"slot" required:"" help:"slot name"`
	JSON bool   `name:"json" help:"emit JSON"`
}

func (c *SwarmTasksCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()
	return c.run(ctx, info)
}

func (c *SwarmTasksCmd) run(ctx context.Context, info workspace.Info) error {
	mgr, closeNC, err := openManager(ctx, info)
	if err != nil {
		return fmt.Errorf("open tasks manager: %w", err)
	}
	defer closeNC()
	defer func() { _ = mgr.Close() }()

	co, err := coord.Open(ctx, newCoordConfig(info))
	if err != nil {
		return fmt.Errorf("open coord: %w", err)
	}
	defer func() { _ = co.Close() }()

	readies, err := co.Ready(ctx)
	if err != nil {
		return fmt.Errorf("coord ready: %w", err)
	}
	readyIDs := make(map[string]struct{}, len(readies))
	for _, r := range readies {
		readyIDs[string(r.ID())] = struct{}{}
	}
	all, err := mgr.List(ctx)
	if err != nil {
		return fmt.Errorf("tasks list: %w", err)
	}
	out := make([]tasks.Task, 0)
	for _, t := range all {
		if _, ok := readyIDs[t.ID]; !ok {
			continue
		}
		if !taskBelongsToSlot(t, c.Slot) {
			continue
		}
		out = append(out, t)
	}
	if c.JSON {
		return emitJSON(os.Stdout, out)
	}
	for _, t := range out {
		fmt.Println(formatListLine(t))
	}
	return nil
}

// taskBelongsToSlot returns true if t can plausibly be claimed by
// agents working on slot. The match is intentionally generous so a
// plan whose tasks lack explicit `Context["slot"]` annotations still
// surfaces here once they have files in the slot's directory.
func taskBelongsToSlot(t tasks.Task, slot string) bool {
	if v := t.Context["slot"]; v == slot {
		return true
	}
	if strings.Contains(t.Title, "[slot: "+slot+"]") {
		return true
	}
	for _, f := range t.Files {
		if strings.HasPrefix(f, slot+"/") || strings.Contains(f, "/"+slot+"/") {
			return true
		}
	}
	return false
}
