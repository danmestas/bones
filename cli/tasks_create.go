package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"
	"github.com/google/uuid"

	"github.com/danmestas/bones/internal/tasks"
)

// TasksCreateCmd creates a new task.
type TasksCreateCmd struct {
	Title      string   `arg:"" help:"task title"`
	Files      string   `name:"files" help:"comma-separated file list"`
	Parent     string   `name:"parent" help:"parent task id"`
	DeferUntil string   `name:"defer-until" help:"RFC3339 time"`
	Context    []string `name:"context" help:"key=value (repeatable)" sep:"none"`
	JSON       bool     `name:"json" help:"emit JSON"`
}

func (c *TasksCreateCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "create", func(ctx context.Context) error {
		if err := validateContextPairs(c.Context); err != nil {
			return err
		}
		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()

		parsedDeferUntil, err := parseRFC3339Flag("defer-until", c.DeferUntil)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		t := tasks.Task{
			ID:            uuid.NewString(),
			Title:         c.Title,
			Status:        tasks.StatusOpen,
			Files:         splitFiles(c.Files),
			Parent:        c.Parent,
			DeferUntil:    parsedDeferUntil,
			Context:       applyContext(nil, c.Context),
			CreatedAt:     now,
			UpdatedAt:     now,
			SchemaVersion: tasks.SchemaVersion,
		}
		if err := mgr.Create(ctx, t); err != nil {
			return err
		}
		if c.JSON {
			return emitJSON(os.Stdout, t)
		}
		fmt.Println(t.ID)
		return nil
	}))
}
