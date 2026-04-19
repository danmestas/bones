package coord

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/agent-infra/internal/assert"
)

// AskAdmin sends a synchronous question to a peer agent after first
// pre-flighting the recipient against the project's presence bucket.
// If the pre-flight finds no live presence entry for recipient,
// AskAdmin returns ErrAgentOffline without touching the chat
// substrate. When the pre-flight succeeds, AskAdmin delegates to the
// same chat.Request path as Ask — same subject shape
// (<proj>.ask.<recipient>), same reply-wait semantics, same error
// translation table.
//
// The "Admin" naming is a holdover from ADR 0008's scope-out language
// around admin-override semantics. Any agent may call AskAdmin; there
// is no role-based restriction in Phase 4. Role-based auth is deferred
// to Phase 6+.
//
// ErrAgentOffline vs. ErrAskTimeout: the pre-flight sentinel narrows
// the "no one was listening at call time" branch to a directory-based
// answer, so callers that want to distinguish "recipient does not
// exist" from "recipient is slow" have a machine-checkable boundary.
// A successful pre-flight that then times out returns ErrAskTimeout
// as usual — presence entries can age out between pre-flight and
// publish, and the timeout path remains the source of truth for
// substrate-observed non-delivery. Callers that do not want the
// pre-flight cost continue to use Ask.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed). Recipient and question
// non-empty preconditions likewise panic.
//
// Operator errors returned:
//
//	ErrAgentOffline — presence pre-flight found no live entry for
//	    recipient in this project. Distinct from ErrAskTimeout.
//	ErrAskTimeout — pre-flight succeeded but the reply-wait
//	    deadline elapsed before a response arrived. Same semantics
//	    as Ask's ErrAskTimeout.
//	context.Canceled — ctx was canceled (not deadlined) before or
//	    during the call. Surfaces wrapped with the coord.AskAdmin
//	    prefix. Distinct from ErrAskTimeout.
//	Any other substrate error — e.g. a presence Get failure or a
//	    NATS publish failure — is wrapped with the coord.AskAdmin
//	    prefix and returned verbatim.
func (c *Coord) AskAdmin(
	ctx context.Context, recipient string, question string,
) (string, error) {
	c.assertOpen("AskAdmin")
	assert.NotNil(ctx, "coord.AskAdmin: ctx is nil")
	assert.NotEmpty(recipient, "coord.AskAdmin: recipient is empty")
	assert.NotEmpty(question, "coord.AskAdmin: question is empty")
	if errors.Is(ctx.Err(), context.Canceled) {
		return "", fmt.Errorf("coord.AskAdmin: %w", context.Canceled)
	}
	present, err := c.sub.presence.Present(ctx, recipient)
	if err != nil {
		return "", fmt.Errorf("coord.AskAdmin: %w", err)
	}
	if !present {
		return "", fmt.Errorf("coord.AskAdmin: %w", ErrAgentOffline)
	}
	subject := projectPrefix(c.cfg.AgentID) + ".ask." + recipient
	reply, err := c.sub.chat.Request(ctx, subject, []byte(question))
	if err != nil {
		return "", translateAskErr("coord.AskAdmin", err)
	}
	return string(reply), nil
}
