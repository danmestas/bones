package coord

import (
	"context"
	"fmt"
	"sync"

	"github.com/dmestas/edgesync/leaf/agent/notify"

	"github.com/danmestas/agent-infra/internal/assert"
)

// Subscribe returns a channel of coord.Event values for messages that
// match pattern, plus a close closure the caller must invoke to tear
// the subscription down. Phase 3 ships ChatMessage as the only concrete
// event type; consumers read with a type switch per ADR 0008.
//
// pattern is the coord-facing thread filter. An empty pattern selects
// every thread in this agent's project (the documented project-wide
// path, used by the 2w5 smoke test); the non-empty pattern is a
// caller-supplied thread name, mapped deterministically to a notify
// ThreadShort via SHA-256 — two Coords watching the same name
// subscribe to the same stream (see ADR 0008's 2026-04-19 update).
// Glob patterns are a Phase 4 follow-up.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed). pattern is NOT asserted
// non-empty — empty is valid and selects the project-wide stream.
//
// Runtime enforcement:
//
//	ErrTooManySubscribers — returned if the live-subscription count on
//	    this Coord already equals Config.MaxSubscribers at entry. The
//	    caller may retry after an existing subscription's close closure
//	    has run (invariant 17 ensures decrement happens exactly once).
//
// The returned close closure cancels an internal ctx derived from the
// caller's ctx, waits for the relay goroutine to drain, and decrements
// the live-subscription counter. The Event channel itself is closed by
// the relay goroutine as it exits, so both the explicit-close and the
// caller-ctx-canceled paths funnel through the same close site — no
// second close(chan) call is needed here, and the channel is never
// double-closed. sync.Once-guarded so subsequent calls return nil and
// take no action (invariant 17).
func (c *Coord) Subscribe(
	ctx context.Context, pattern string,
) (<-chan Event, func() error, error) {
	c.assertOpen("Subscribe")
	assert.NotNil(ctx, "coord.Subscribe: ctx is nil")
	if err := c.reserveSubscriberSlot(); err != nil {
		return nil, nil, err
	}
	relayCtx, cancel := context.WithCancel(ctx)
	src := c.watchChat(relayCtx, pattern)
	out := make(chan Event, 16)
	done := make(chan struct{})
	go c.relaySubscribe(src, out, done)
	closer := c.subscribeCloser(cancel, done)
	return out, closer, nil
}

// watchChat returns a <-chan notify.Message matching pattern. An empty
// pattern routes through chat.WatchAll (project-wide wildcard); a
// non-empty pattern is passed through to chat.Watch as a caller
// thread name — chat.Watch hashes it into the same deterministic
// ThreadShort that chat.Send uses, so publishers and subscribers
// converge on one NATS subject without coordination. Extracted so
// Subscribe stays below the 70-line funlen cap and so the
// empty-is-project-wide branch has a single home.
func (c *Coord) watchChat(
	ctx context.Context, pattern string,
) <-chan notify.Message {
	if pattern == "" {
		return c.chat.WatchAll(ctx)
	}
	return c.chat.Watch(ctx, pattern)
}

// reserveSubscriberSlot increments subsActive and rolls back if the new
// count exceeds Config.MaxSubscribers, returning ErrTooManySubscribers.
// Extracted so Subscribe itself stays below the 70-line funlen cap.
func (c *Coord) reserveSubscriberSlot() error {
	next := c.subsActive.Add(1)
	if int(next) > c.cfg.MaxSubscribers {
		c.subsActive.Add(-1)
		return fmt.Errorf(
			"coord.Subscribe: %w", ErrTooManySubscribers,
		)
	}
	return nil
}

// relaySubscribe translates notify.Message values from src into coord
// events on out. It is the sole owner of out's close: when src closes
// (caller's ctx canceled OR explicit close closure ran), the relay
// closes out and signals done so the close closure knows the goroutine
// has exited. Two paths trigger src's close — caller ctx cancellation
// cascades through the relayCtx derived in Subscribe, and the close
// closure cancels that same relayCtx directly. Either path funnels
// through this single close(out), so invariant 17 is preserved without
// a second sync.Once on the channel.
func (c *Coord) relaySubscribe(
	src <-chan notify.Message,
	out chan<- Event,
	done chan<- struct{},
) {
	defer close(done)
	defer close(out)
	for msg := range src {
		evt := eventFromMessage(msg)
		select {
		case out <- evt:
		default:
			// Slow consumer: drop to keep the relay moving. The
			// notify substrate is read-now-or-miss-now by design
			// (ADR 0008); dropping here preserves that posture and
			// avoids blocking the chat Watch goroutine upstream.
		}
	}
}

// subscribeCloser returns the caller-visible close closure. Idempotent
// via sync.Once (invariant 17): the first call cancels the internal
// ctx, waits for the relay goroutine to drain (which closes the event
// channel), and decrements the live-subscription counter; subsequent
// calls return nil and take no action.
//
// The event-channel close lives in the relay goroutine, not here, so
// the caller-ctx cancellation path (which never runs this closure)
// still closes the channel. This closure's work is the explicit-close
// bookkeeping: cancel the relay ctx, wait for drain, and free the
// subscriber slot.
func (c *Coord) subscribeCloser(
	cancel context.CancelFunc,
	done <-chan struct{},
) func() error {
	var once sync.Once
	return func() error {
		once.Do(func() {
			cancel()
			<-done
			c.subsActive.Add(-1)
		})
		return nil
	}
}
