package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/timefmt"
)

// migrationMarkerTaskID is the synthetic ID recovery uses to record
// that a v0.12 → v0.13 KV→event-log migration ran. The marker emits as
// a Created event but is internal bookkeeping; user-facing surfaces
// (watch, both human and JSON) filter it out.
const migrationMarkerTaskID = "__bones_tasks_events_migrated"

// TasksWatchCmd subscribes to the task event log and streams lifecycle
// events to stdout until Ctrl-C or context cancellation. Default is
// live-only per ADR 0052; --from and --since opt into a one-shot
// backfill before tailing. --json switches the output to one
// EventEnvelope JSON object per line for downstream automation.
type TasksWatchCmd struct {
	From uint64 `name:"from" help:"start at stream sequence (excl with --since)"`
	// Since takes a Go time.Duration (e.g. "24h"). Mutually exclusive
	// with --from. Default zero means live-only consumption.
	Since time.Duration `name:"since" help:"start at wall-clock offset (excl with --from)"`
	JSON  bool          `name:"json" help:"emit one EventEnvelope JSON object per line"`
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

	emit := watchEmitter(c.JSON)

	// One-shot backfill if --from or --since is set; happens before the
	// live tail so operators see history then live updates. Backfill
	// uses Replay (one-shot drain), live uses the new event-log
	// subscription primitive (ADR 0052 follow-on, #343).
	if err := watchBackfill(ctx, m, c.From, c.Since, emit); err != nil {
		return err
	}

	ch, err := m.Live(ctx)
	if err != nil {
		return fmt.Errorf("watch: subscribe: %w", err)
	}

	if !c.JSON {
		fmt.Fprintln(os.Stderr, "watching task events — press Ctrl-C to stop")
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case env, ok := <-ch:
			if !ok {
				return nil
			}
			if env.TaskID == migrationMarkerTaskID {
				continue
			}
			emit(env)
		}
	}
}

// watchEmitter returns the formatter used for both backfill and live
// output. Two shapes: human (printEventEnvelope) or JSONL (one encoded
// EventEnvelope per line). The switch happens once at the top of Run
// so the loop body stays branch-free.
func watchEmitter(asJSON bool) func(tasks.EventEnvelope) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		return func(env tasks.EventEnvelope) {
			_ = enc.Encode(env)
		}
	}
	return printEventEnvelope
}

// watchBackfill runs Replay with --from / --since options and emits
// each event through the provided emitter before the live tail starts.
// Default behavior (both flags zero) is live-only; nothing prints
// until the first live event. The migration marker is filtered here
// too so backfill output mirrors live behavior.
func watchBackfill(
	ctx context.Context,
	m *tasks.Manager,
	from uint64,
	since time.Duration,
	emit func(tasks.EventEnvelope),
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
		if env.TaskID == migrationMarkerTaskID {
			continue
		}
		emit(env)
	}
	return nil
}

// printEventEnvelope renders a single event-log envelope in human-
// readable form. The bracket prefix is timefmt.Display per #324 —
// operator surface, local time + zone abbreviation. Both backfill and
// live paths route through this helper after the event-log migration
// (#343); the prior printTaskEvent (KV-watch shape, time.Now()
// timestamps) is removed.
func printEventEnvelope(env tasks.EventEnvelope) {
	ts := timefmt.Display(env.Timestamp.Time)
	fmt.Printf("[%s] %-12s id=%s seq=%d\n",
		ts, env.Type.String(), env.TaskID, env.StreamSeq)
}
