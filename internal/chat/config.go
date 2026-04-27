// Package chat is the substrate layer that backs coord's Post, Ask, and
// Subscribe on top of EdgeSync's notify service. Send and Watch route
// through notify.Service; Request uses raw NATS request/reply on the
// ask subject family. See docs/adr/0008-chat-substrate.md for the
// decision record covering why chat carries two substrates rather
// than one.
//
// This package is internal and unexported: callers outside
// github.com/danmestas/bones must not depend on it. The
// notify.Message type leaks across the package boundary into coord
// where eventFromMessage translates it into coord.ChatMessage per
// ADR 0003's substrate-hiding rule; no notify type appears on any
// public coord signature.
package chat

import (
	"fmt"

	"github.com/nats-io/nats.go"
)

// Config configures Open. Every field is required; there are no silent
// defaults. The operator supplies the numbers — coord.Open is the
// enforcement point for Config.Validate and propagates its own
// validated inputs into this struct.
type Config struct {
	// AgentID identifies this chat instance across the substrate. It is
	// threaded through to outgoing notify.Message as the From field so
	// receivers can attribute messages back to a sender.
	AgentID string

	// ProjectPrefix is the <proj> segment used to build notify subjects
	// (notify.<proj>.<thread>) and ask subjects (<proj>.ask.<recipient>).
	// Derived at the coord layer from coord.Config.AgentID per ADR 0008;
	// the chat package takes it as pre-derived input.
	ProjectPrefix string

	// Nats is the pre-connected NATS handle from coord. The chat manager
	// does not dial its own connection — it shares the one coord opened
	// so reconnection policy, auth, and TLS remain a single-source
	// concern in coord.Config. The handle is used directly for Ask's
	// request/reply path and handed to notify.NewService for Send/Watch.
	Nats *nats.Conn

	// FossilRepoPath is the filesystem path at which notify.Service
	// persists its message log. The chat manager opens (or creates) the
	// Fossil repo at this path during Open and closes it during Close,
	// mirroring the ownership posture that internal/holds takes for its
	// JetStream KV bucket.
	FossilRepoPath string

	// MaxSubscribers caps the number of concurrent Watch callers coord
	// will hand out. Validated here so an obviously-broken value fails
	// at Open rather than at first subscribe; runtime enforcement ships
	// with Phase 3D when the Subscribe surface lands.
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
	if c.FossilRepoPath == "" {
		return fmt.Errorf(
			"chat.Config: FossilRepoPath: must be non-empty",
		)
	}
	if c.MaxSubscribers <= 0 {
		return fmt.Errorf(
			"chat.Config: MaxSubscribers: must be > 0",
		)
	}
	return nil
}
