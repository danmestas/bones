package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/tasks"
)

// TasksWatchCmd subscribes to the tasks KV bucket and streams human-readable
// lifecycle events to stdout until Ctrl-C or context cancellation.
type TasksWatchCmd struct{}

func (c *TasksWatchCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	nc, err := nats.Connect(info.NATSURL)
	if err != nil {
		return fmt.Errorf("watch: nats connect: %w", err)
	}
	defer nc.Close()

	m, err := tasks.Open(ctx, nc, tasks.Config{
		BucketName:   tasks.DefaultBucketName,
		HistoryDepth: 10,
		MaxValueSize: 64 * 1024,
		ChanBuffer:   64,
	})
	if err != nil {
		return fmt.Errorf("watch: tasks.Open: %w", err)
	}
	defer func() { _ = m.Close() }()

	ch, err := m.Watch(ctx)
	if err != nil {
		return fmt.Errorf("watch: subscribe: %w", err)
	}

	fmt.Fprintln(os.Stderr, "watching task events — press Ctrl-C to stop")

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			printTaskEvent(ev)
		}
	}
}

// printTaskEvent formats a single tasks.Event as a timestamped line.
func printTaskEvent(ev tasks.Event) {
	ts := time.Now().Format("15:04:05")
	switch ev.Kind {
	case tasks.EventCreated:
		fmt.Printf("[%s] created  task=%q id=%s\n", ts, ev.Task.Title, ev.ID)
	case tasks.EventUpdated:
		switch ev.Task.Status {
		case tasks.StatusClaimed:
			fmt.Printf("[%s] claimed  task=%q by=%s id=%s\n",
				ts, ev.Task.Title, ev.Task.ClaimedBy, ev.ID)
		case tasks.StatusClosed:
			fmt.Printf("[%s] closed   task=%q id=%s\n",
				ts, ev.Task.Title, ev.ID)
		default:
			fmt.Printf("[%s] updated  task=%q status=%s id=%s\n",
				ts, ev.Task.Title, ev.Task.Status, ev.ID)
		}
	case tasks.EventDeleted:
		fmt.Printf("[%s] deleted  id=%s\n", ts, ev.ID)
	}
}
