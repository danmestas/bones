package coord

import (
	"context"

	"github.com/danmestas/bones/internal/assert"
)

// SubscribePattern returns a channel of coord.Event values for every
// thread whose ThreadShort matches the given NATS subject-segment
// pattern, plus a close closure the caller must invoke to tear the
// subscription down. Patterns use NATS subject-wildcard syntax: "*"
// matches every ThreadShort (equivalent to Subscribe(ctx, "") on the
// project-wide path), ">" matches every ThreadShort plus any
// hypothetical subsegments, and a literal pattern matches a single
// ThreadShort — usefully, the string returned by ChatMessage.Thread()
// on an earlier event, so a consumer can bootstrap a pattern
// subscription from observed traffic without round-tripping through
// the thread-name hash.
//
// Unlike Subscribe, SubscribePattern does NOT hash its pattern —
// callers supply a raw substrate-level pattern. This is the
// deliberate substrate leak called out in ADR 0009's Open Questions
// (ticket agent-infra-6wv): option 1 lands a Phase-4 glob-Subscribe
// commitment without the new KV registry bucket and per-Post write
// cost of option 3. Callers that need name-level pattern matching
// (e.g. "deploy.*" matching thread names) must layer a registry
// themselves or wait for a future option-3 ticket.
//
// pattern is asserted non-empty to keep the "project-wide" path on
// Subscribe(ctx, "") and the "pattern" path on SubscribePattern.
// Callers wanting everything should prefer Subscribe's empty-pattern
// shorthand over SubscribePattern("*") — the two subscriptions are
// behaviorally identical but Subscribe is the documented entry point
// for the wildcard-all case.
//
// Invariants asserted: 1 (ctx non-nil), 8 (Coord not closed). Pattern
// non-empty panics before any substrate work.
//
// Runtime enforcement:
//
//	ErrTooManySubscribers — returned if the live-subscription count on
//	    this Coord already equals Config.MaxSubscribers at entry.
//	    Shared with Subscribe; the slot is freed by the close closure
//	    (invariant 17).
//
// The returned close closure shares the cancel/drain/decrement shape
// of Subscribe's closer — sync.Once-guarded and safe to call from
// multiple goroutines.
func (c *Coord) SubscribePattern(
	ctx context.Context, pattern string,
) (<-chan Event, func() error, error) {
	c.assertOpen("SubscribePattern")
	assert.NotNil(ctx, "coord.SubscribePattern: ctx is nil")
	assert.NotEmpty(pattern, "coord.SubscribePattern: pattern is empty")
	if err := c.reserveSubscriberSlot("coord.SubscribePattern"); err != nil {
		return nil, nil, err
	}
	relayCtx, cancel := context.WithCancel(ctx)
	src := c.sub.chat.WatchPattern(relayCtx, pattern)
	out := make(chan Event, 16)
	done := make(chan struct{})
	go c.relaySubscribe(src, out, done)
	closer := c.subscribeCloser(cancel, done)
	return out, closer, nil
}
