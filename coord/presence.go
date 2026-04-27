package coord

import (
	"context"
	"fmt"
	"sync"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/presence"
)

// Who returns the live-presence snapshot for this Coord's project.
// Each Presence describes one agent whose heartbeat KV entry is still
// current (not yet TTL-expired). The list includes this Coord itself —
// Open writes our initial entry before returning, so the caller's own
// Coord is always visible in Who by the time Who can be called.
//
// Project scoping matches the Post/Ask scheme: agents in other
// projects are not surfaced. This is a fresh read; presence state is
// read-through the KV with no client-side caching.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed).
//
// Operator errors returned: any substrate error from the KV list/scan
// path, wrapped with the coord.Who prefix.
func (c *Coord) Who(ctx context.Context) ([]Presence, error) {
	c.assertOpen("Who")
	assert.NotNil(ctx, "coord.Who: ctx is nil")
	entries, err := c.sub.presence.Who(ctx)
	if err != nil {
		return nil, fmt.Errorf("coord.Who: %w", err)
	}
	out := make([]Presence, 0, len(entries))
	for _, e := range entries {
		out = append(out, presenceFromEntry(e))
	}
	return out, nil
}

// WatchPresence returns a channel of coord.Event values that fire
// whenever an agent comes online or goes offline in this Coord's
// project. Concrete type is PresenceChange. Consumers discriminate
// via the standard Event type switch (ADR 0008); mixing chat and
// presence events on the same switch is permitted because the
// sealed interface carries both types.
//
// The initial snapshot is NOT replayed: Watch starts from the moment
// of subscription. Use Who for a snapshot; use WatchPresence for
// deltas. Consumers that want both sequence them explicitly.
//
// The returned close closure is idempotent per invariant 17 — the
// first call cancels the internal ctx, waits for the relay goroutine
// to drain, and closes the event channel; subsequent calls return
// nil and take no action.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed).
//
// Operator errors returned: any substrate error from the KV watch
// path, wrapped with the coord.WatchPresence prefix.
func (c *Coord) WatchPresence(
	ctx context.Context,
) (<-chan Event, func() error, error) {
	c.assertOpen("WatchPresence")
	assert.NotNil(ctx, "coord.WatchPresence: ctx is nil")
	relayCtx, cancel := context.WithCancel(ctx)
	src, err := c.sub.presence.Watch(relayCtx)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("coord.WatchPresence: %w", err)
	}
	out := make(chan Event, 16)
	done := make(chan struct{})
	go relayPresence(src, out, done)
	closer := presenceCloser(cancel, done)
	return out, closer, nil
}

// relayPresence translates presence.Event values from src into coord
// events on out. It is the sole owner of out's close: when src closes
// (caller's ctx canceled OR explicit close closure ran), the relay
// closes out and signals done so the close closure knows the goroutine
// has exited. Mirrors relaySubscribe in subscribe.go — same structure,
// different event type.
func relayPresence(
	src <-chan presence.Event,
	out chan<- Event,
	done chan<- struct{},
) {
	defer close(done)
	defer close(out)
	for e := range src {
		evt := PresenceChange{
			agentID:   e.AgentID,
			project:   e.Project,
			up:        e.Kind == presence.EventUp,
			timestamp: e.Timestamp,
		}
		select {
		case out <- evt:
		default:
			// Slow consumer: drop to keep the relay moving. Matches
			// the chat-Subscribe drop posture — presence is delta
			// state, and the next heartbeat/TTL cycle will re-surface
			// the current truth.
		}
	}
}

// presenceCloser returns the caller-visible close closure for
// WatchPresence. Idempotent via sync.Once (invariant 17): the first
// call cancels the internal ctx (which closes the source channel,
// which triggers the relay's close of out), waits for the relay
// goroutine to drain, and returns. Subsequent calls return nil and
// take no action.
func presenceCloser(
	cancel context.CancelFunc, done <-chan struct{},
) func() error {
	var once sync.Once
	return func() error {
		once.Do(func() {
			cancel()
			<-done
		})
		return nil
	}
}

// _ is a compile-time assertion that PresenceChange satisfies Event.
// A failing assertion here means we lost the sealed-interface seal,
// which would let external packages construct PresenceChange values
// and defeat ADR 0003's substrate-hiding posture.
var _ Event = PresenceChange{}
