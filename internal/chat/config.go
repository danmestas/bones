// Package chat is the substrate layer that backs coord's Post, Ask,
// and Subscribe on top of NATS JetStream. Send and Watch route through
// a per-project JetStream stream (chat-<proj> with subjects
// chat.<proj>.>); Request uses raw NATS request/reply on the ask
// subject family. See docs/adr/0047-chat-on-jetstream.md for the
// decision record covering why chat lives on a JetStream stream and
// not on EdgeSync notify + libfossil.
//
// This package is internal and unexported: callers outside
// github.com/danmestas/bones must not depend on it. The Envelope type
// crosses the package boundary into coord where eventFromEnvelope
// translates it into coord.ChatMessage per ADR 0003's substrate-hiding
// rule; no chat type appears on any public coord signature.
package chat

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// Config configures Open. The operator supplies the identity/routing
// fields; MaxRetentionAge is optional (0 = unbounded).
type Config struct {
	// AgentID identifies this chat instance across the substrate. It is
	// threaded through to outgoing Envelope as the From field so
	// receivers can attribute messages back to a sender.
	AgentID string

	// ProjectPrefix is the <proj> segment used to build chat subjects
	// (chat.<proj>.<short>) and ask subjects (<proj>.ask.<recipient>).
	// Derived at the coord layer from coord.Config.AgentID per ADR 0008;
	// the chat package takes it as pre-derived input.
	ProjectPrefix string

	// Nats is the pre-connected NATS handle from coord. The chat manager
	// does not dial its own connection — it shares the one coord opened
	// so reconnection policy, auth, and TLS remain a single-source
	// concern in coord.Config. The handle is used directly for Ask's
	// request/reply path and to construct the JetStream context for
	// Send/Watch.
	Nats *nats.Conn

	// MaxRetentionAge bounds the JetStream stream's MaxAge. Zero means
	// unbounded — chat history persists until disk is full or the
	// operator runs `nats stream purge`. Per ADR 0047 the default is
	// unbounded so coord.Prime preserves agent context across long
	// absences (ADR 0036).
	MaxRetentionAge time.Duration

	// MaxSubscribers caps the number of concurrent Watch callers coord
	// will hand out. Validated here so an obviously-broken value fails
	// at Open rather than at first subscribe.
	MaxSubscribers int
}

// Validate checks every Config field against its documented bounds and
// returns the first violation as an error. The error message follows
// the shape "chat.Config: <field>: <reason>". Validate is pure; it
// does not panic on bad operator input per invariant 9 — panics are
// reserved for programmer-error invariants inside Open and the
// wrappers.
func (c Config) Validate() error {
	if c.AgentID == "" {
		return fmt.Errorf("chat.Config: AgentID: must be non-empty")
	}
	if c.ProjectPrefix == "" {
		return fmt.Errorf(
			"chat.Config: ProjectPrefix: must be non-empty",
		)
	}
	if c.Nats == nil {
		return fmt.Errorf("chat.Config: Nats: must be non-nil")
	}
	if c.MaxRetentionAge < 0 {
		return fmt.Errorf(
			"chat.Config: MaxRetentionAge: must be >= 0 (0 = unbounded)",
		)
	}
	if c.MaxSubscribers <= 0 {
		return fmt.Errorf(
			"chat.Config: MaxSubscribers: must be > 0",
		)
	}
	return nil
}
