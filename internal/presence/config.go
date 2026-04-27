// Package presence is the substrate layer that backs coord's Who and
// WatchPresence. A single JetStream KV bucket carries one entry per
// live agent, refreshed on Config.HeartbeatInterval cadence. Entry TTL
// is 3x HeartbeatInterval per ADR 0009 invariant 19 — tightening the
// multiplier requires an ADR amendment.
//
// This package is internal and unexported: callers outside
// github.com/danmestas/bones must not depend on it. The internal
// Entry and Event types translate through coord/types.go and
// coord/events.go into the public Presence DTO and PresenceChange
// event per ADR 0003's substrate-hiding rule.
package presence

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// Config configures Open. Every field is required; there are no silent
// defaults. The operator supplies the numbers — coord.Open is the
// enforcement point for coord.Config.Validate and propagates its own
// validated inputs into this struct.
type Config struct {
	// AgentID identifies this coord instance across the substrate. It
	// is threaded into the presence Entry's AgentID field and used to
	// derive the KV key this Manager refreshes on heartbeat.
	AgentID string

	// Project is the <proj> segment used to scope presence queries.
	// Matches the project-prefix scheme Post/Ask use for NATS subjects
	// (ADR 0008). Presence is project-scoped per ADR 0009: agents in
	// project A cannot see agents in project B.
	Project string

	// Bucket is the name of the JetStream KV bucket backing presence.
	// Coord supplies bones-presence; validated here so a
	// misconfiguration (empty) fails at Open rather than at first Put.
	Bucket string

	// NATSConn is the pre-connected NATS handle from coord. The
	// presence manager does not dial its own connection — it shares
	// the one coord opened.
	NATSConn *nats.Conn

	// HeartbeatInterval is the cadence at which the heartbeat goroutine
	// refreshes this agent's KV entry. Bucket TTL is 3x this value per
	// ADR 0009 invariant 19; the multiplier is fixed in code, not
	// configurable.
	HeartbeatInterval time.Duration

	// ChanBuffer sets the channel buffer for Watch. If left zero, Open
	// substitutes defaultChanBuffer.
	ChanBuffer int
}

// Validate checks every Config field against its documented bounds and
// returns the first violation as an error. The error message follows
// the shape "presence.Config: <field>: <reason>". Validate is pure; it
// does not panic on bad operator input per invariant 9 — panics are
// reserved for programmer-error invariants inside Open and the method
// wrappers.
func (c Config) Validate() error {
	if c.AgentID == "" {
		return fmt.Errorf("presence.Config: AgentID: must be non-empty")
	}
	if c.Project == "" {
		return fmt.Errorf("presence.Config: Project: must be non-empty")
	}
	if c.Bucket == "" {
		return fmt.Errorf("presence.Config: Bucket: must be non-empty")
	}
	if c.NATSConn == nil {
		return fmt.Errorf("presence.Config: NATSConn: must be non-nil")
	}
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf(
			"presence.Config: HeartbeatInterval: must be > 0",
		)
	}
	if c.ChanBuffer < 0 {
		return fmt.Errorf(
			"presence.Config: ChanBuffer: must be >= 0",
		)
	}
	return nil
}

// TTLMultiplier is the fixed multiplier that derives the KV bucket TTL
// from Config.HeartbeatInterval per ADR 0009 invariant 19. Three
// heartbeat intervals give two missed-heartbeat intervals of slack
// before an entry expires, which is the published convention for
// similar liveness systems. Changing this multiplier requires an ADR
// amendment.
const TTLMultiplier = 3
