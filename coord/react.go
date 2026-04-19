package coord

import (
	"context"
	"fmt"

	"github.com/danmestas/agent-infra/internal/assert"
)

// React posts a reaction to the message identified by messageID in the
// given thread. The reaction payload is opaque to coord — emoji, text,
// or arbitrary bytes all pass through untouched — and is surfaced to
// peers subscribed to the same thread as a Reaction event on the
// coord.Subscribe channel.
//
// Per ADR 0009 reactions piggyback on the chat substrate: they are
// encoded in-band as a notify.Message body with the REACT prefix and
// routed through the same chat.Send path as Post. No new KV bucket,
// no new NATS subject. The encoding is a substrate detail and never
// appears on the public surface — callers emit reactions through
// React and receive them through Subscribe as Reaction events.
//
// messageID is whatever ChatMessage.MessageID returned for the target
// message. Coord does not verify that messageID corresponds to a real
// message on the thread: a reaction to a non-existent message will
// still publish, and consumers receive it as a Reaction event with a
// target that matches no visible chat. This is deliberate — the
// alternative would couple reactions to a per-message lookup on the
// write path, which the chat substrate was designed to avoid.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed). Thread, messageID, and
// reaction non-empty preconditions likewise panic.
//
// Operator errors returned: any substrate error from chat.Send, wrapped
// with the coord.React prefix.
func (c *Coord) React(
	ctx context.Context, thread, messageID, reaction string,
) error {
	c.assertOpen("React")
	assert.NotNil(ctx, "coord.React: ctx is nil")
	assert.NotEmpty(thread, "coord.React: thread is empty")
	assert.NotEmpty(messageID, "coord.React: messageID is empty")
	assert.NotEmpty(reaction, "coord.React: reaction is empty")
	body := reactionBodyPrefix + messageID + ":" + reaction
	if err := c.sub.chat.Send(ctx, thread, body); err != nil {
		return fmt.Errorf("coord.React: %w", err)
	}
	return nil
}
