package tasks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/bones/internal/assert"
)

// LogReadOpts configures a one-shot read of the event log via Replay.
// At most one of FromSeq and Since may be set. When both are zero, the
// reader returns the most recent Limit events (Limit defaults to
// RecentActivityCount).
type LogReadOpts struct {
	// FromSeq is the JetStream stream sequence to start at (inclusive).
	// 0 means unset.
	FromSeq uint64

	// Since is a wall-clock offset relative to time.Now(). 0 means
	// unset. Mutually exclusive with FromSeq.
	Since time.Duration

	// Limit caps the returned event count. 0 disables the cap.
	Limit int

	// FilterTaskID, when non-empty, restricts the read to events
	// matching `tasks.events.<task_id>`.
	FilterTaskID string
}

// Replay reads events from the task event log according to opts and
// returns them in stream order. Used by `bones tasks watch`'s
// --from / --since backfill and by `bones status`'s recent-activity
// surface.
//
// Replay is one-shot: it returns once the consumer drains; callers
// wanting live updates use Watch instead.
func (m *Manager) Replay(
	ctx context.Context, opts LogReadOpts,
) ([]EventEnvelope, error) {
	assert.NotNil(ctx, "tasks.Replay: ctx is nil")
	if m.stream == nil {
		return nil, errors.New("tasks.Replay: event log disabled")
	}
	if opts.FromSeq != 0 && opts.Since != 0 {
		return nil, errors.New(
			"tasks.Replay: --from and --since are mutually exclusive",
		)
	}
	cfg, err := buildReplayConsumerCfg(opts)
	if err != nil {
		return nil, err
	}
	cons, err := m.stream.OrderedConsumer(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("tasks.Replay: consumer: %w", err)
	}
	return drainReplay(ctx, cons, opts)
}

// buildReplayConsumerCfg translates LogReadOpts into the
// jetstream.OrderedConsumerConfig the consumer wants. The defaulting
// rules (no flags → "last RecentActivityCount events") are applied
// here so all callers see consistent behavior.
func buildReplayConsumerCfg(
	opts LogReadOpts,
) (jetstream.OrderedConsumerConfig, error) {
	subj := AllEventsSubject
	if opts.FilterTaskID != "" {
		subj = EventSubject(opts.FilterTaskID)
	}
	cfg := jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subj},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	}
	switch {
	case opts.FromSeq != 0:
		cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		cfg.OptStartSeq = opts.FromSeq
	case opts.Since != 0:
		t := time.Now().UTC().Add(-opts.Since)
		cfg.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		cfg.OptStartTime = &t
	}
	return cfg, nil
}

// drainReplay pulls every available message from cons and returns the
// decoded envelopes. Stops when the consumer reports no more messages
// or when the limit is hit.
func drainReplay(
	ctx context.Context,
	cons jetstream.Consumer,
	opts LogReadOpts,
) ([]EventEnvelope, error) {
	const fetchBatch = 100
	const fetchWait = 250 * time.Millisecond
	out := make([]EventEnvelope, 0, fetchBatch)
	for {
		batch, err := cons.Fetch(fetchBatch, jetstream.FetchMaxWait(fetchWait))
		if err != nil {
			return out, err
		}
		empty := true
		for msg := range batch.Messages() {
			empty = false
			env, perr := UnmarshalEnvelope(msg.Data())
			_ = msg.Ack()
			if perr != nil {
				continue
			}
			meta, _ := msg.Metadata()
			if meta != nil {
				env.StreamSeq = meta.Sequence.Stream
			}
			out = append(out, env)
			if opts.Limit > 0 && len(out) >= opts.Limit {
				return out, nil
			}
		}
		if empty {
			return out, nil
		}
		if err := batch.Error(); err != nil {
			return out, err
		}
	}
}

// Live opens a live subscription to the task event log. The returned
// channel emits envelopes published *after* the call (DeliverNewPolicy)
// until ctx is canceled, the manager is closed, or the underlying
// consumer stops. Symmetric to Replay (one-shot drain) but for the
// continuous tail used by `bones tasks watch`.
//
// Per ADR 0052 the event log is the source of truth for task state;
// Live consumers see the full envelope (Type, TaskID, Timestamp,
// Payload) on every event — no projection lookup required. Field-level
// changes for EventTypeUpdated land in the payload, so context-only
// updates surface here even though they don't mutate the KV projection
// shape consumers can distinguish.
//
// Callers must drain the channel promptly; a blocked reader stalls the
// forwarder. Buffer is sized for typical interactive use (64). Decode
// errors are dropped silently — one malformed record does not poison
// the stream.
func (m *Manager) Live(ctx context.Context) (<-chan EventEnvelope, error) {
	assert.NotNil(ctx, "tasks.Live: ctx is nil")
	if m.stream == nil {
		return nil, errors.New("tasks.Live: event log disabled")
	}

	cons, err := m.stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{AllEventsSubject},
		DeliverPolicy:  jetstream.DeliverNewPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("tasks.Live: consumer: %w", err)
	}

	out := make(chan EventEnvelope, 64)
	go forwardLive(ctx, cons, out)
	return out, nil
}

// forwardLive bridges a JetStream consumer to the EventEnvelope channel.
// Callbacks fired by Consume marshal-and-send each envelope; the loop
// blocks on ctx.Done so the goroutine has a single exit path. cc.Stop
// flushes in-flight callbacks before close(out) so a late callback
// never writes to a closed channel.
func forwardLive(
	ctx context.Context,
	cons jetstream.Consumer,
	out chan EventEnvelope,
) {
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		env, perr := UnmarshalEnvelope(msg.Data())
		_ = msg.Ack()
		if perr != nil {
			return
		}
		meta, _ := msg.Metadata()
		if meta != nil {
			env.StreamSeq = meta.Sequence.Stream
		}
		select {
		case out <- env:
		case <-ctx.Done():
		}
	})
	if err != nil {
		close(out)
		return
	}

	<-ctx.Done()
	cc.Stop()
	close(out)
}

// Recent returns the most recent n events from the log in stream
// order (oldest of the slice first, newest last). Used by
// `bones status` for the Recent Activity surface.
func (m *Manager) Recent(
	ctx context.Context, n int,
) ([]EventEnvelope, error) {
	if n <= 0 {
		n = RecentActivityCount
	}
	all, err := m.Replay(ctx, LogReadOpts{})
	if err != nil {
		return nil, err
	}
	if len(all) <= n {
		return all, nil
	}
	return all[len(all)-n:], nil
}
