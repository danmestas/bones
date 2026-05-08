package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/timefmt"
)

// TasksWatchCmd subscribes to the task event log and streams human-
// readable lifecycle events to stdout until Ctrl-C or context
// cancellation. Default is live-only per ADR 0052; --from and --since
// opt into a one-shot backfill before tailing.
type TasksWatchCmd struct {
	From uint64 `name:"from" help:"start at stream sequence (excl with --since)"`
	// Since takes a Go time.Duration (e.g. "24h"). Mutually exclusive
	// with --from. Default zero means live-only consumption.
	Since time.Duration `name:"since" help:"start at wall-clock offset (excl with --from)"`
}

func (c *TasksWatchCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	if c.From != 0 && c.Since != 0 {
		return errors.New("watch: --from and --since are mutually exclusive")
	}

	nc, err := nats.Connect(info.NATSURL)
	if err != nil {
		return fmt.Errorf("watch: nats connect: %w", err)
	}
	defer nc.Close()

	m, err := tasks.Open(ctx, nc, tasks.Config{
		BucketName:     tasks.DefaultBucketName,
		HistoryDepth:   10,
		MaxValueSize:   64 * 1024,
		ChanBuffer:     64,
		EnableEventLog: true,
	})
	if err != nil {
		return fmt.Errorf("watch: tasks.Open: %w", err)
	}
	defer func() { _ = m.Close() }()

	// One-shot backfill if --from or --since is set; happens before the
	// live KV-watch tail so operators see history then live updates.
	if err := watchBackfill(ctx, m, c.From, c.Since); err != nil {
		return err
	}

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

// watchBackfill runs Replay with --from / --since options and renders
// each event before the live tail starts. Default behavior (both flags
// zero) is live-only; nothing prints until the first live event.
func watchBackfill(
	ctx context.Context,
	m *tasks.Manager,
	from uint64,
	since time.Duration,
) error {
	if from == 0 && since == 0 {
		return nil
	}
	envs, err := m.Replay(ctx, tasks.LogReadOpts{
		FromSeq: from,
		Since:   since,
	})
	if err != nil {
		return fmt.Errorf("watch: backfill: %w", err)
	}
	for _, env := range envs {
		printEventEnvelope(env)
	}
	return nil
}

// printEventEnvelope renders a single event-log envelope in the same
// shape as printTaskEvent so live and backfill output align. The
// bracket prefix is timefmt.Display per #324 — live operator surface,
// local time + zone abbreviation.
func printEventEnvelope(env tasks.EventEnvelope) {
	ts := timefmt.Display(env.Timestamp)
	fmt.Printf("[%s] %-12s id=%s seq=%d\n",
		ts, env.Type.String(), env.TaskID, env.StreamSeq)
}

// printTaskEvent formats a single tasks.Event as a timestamped line.
// Retained for the live KV-watch path; the event-log path uses
// printEventEnvelope above.
func printTaskEvent(ev tasks.Event) {
	ts := timefmt.Display(time.Now())
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
