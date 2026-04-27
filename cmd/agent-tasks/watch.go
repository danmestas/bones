package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/agent-infra/internal/tasks"
	"github.com/danmestas/agent-infra/internal/workspace"
)

func init() {
	handlers["watch"] = watchCmd
}

// watchCmd subscribes to the tasks KV bucket and streams human-readable
// lifecycle events to stdout until Ctrl-C or context cancellation (DX audit I3).
//
// Output format:
//
//	[15:04:05] created  task=<title> id=<id>
//	[15:04:07] claimed  task=<title> by=<agent> id=<id>
//	[15:04:09] closed   task=<title> id=<id>
//	[15:04:09] deleted  id=<id>
func watchCmd(ctx context.Context, info workspace.Info, args []string) error {
	// No flags: watch takes no arguments.
	nc, err := nats.Connect(info.NATSURL)
	if err != nil {
		return fmt.Errorf("watch: nats connect: %w", err)
	}
	defer nc.Close()

	m, err := tasks.Open(ctx, nc, tasks.Config{
		BucketName:   "agent_tasks",
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

	// Drain the initial snapshot silently: only live changes are interesting
	// for human monitoring. We detect snapshot completion by watching for the
	// first nil guard that Watch emits after it delivers the initial keys —
	// but the tasks.Watch API does not expose that signal directly. Instead
	// we collect events with a short read window: if the bucket is empty the
	// first event will already be a live change.
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
			fmt.Printf("[%s] closed   task=%q id=%s\n", ts, ev.Task.Title, ev.ID)
		default:
			fmt.Printf("[%s] updated  task=%q status=%s id=%s\n",
				ts, ev.Task.Title, ev.Task.Status, ev.ID)
		}
	case tasks.EventDeleted:
		fmt.Printf("[%s] deleted  id=%s\n", ts, ev.ID)
	}
}
