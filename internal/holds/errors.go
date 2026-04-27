// Package holds is the substrate layer that stores file-level holds
// in a NATS JetStream KV bucket. It exposes four primitives — Announce,
// Release, WhoHas, Subscribe — consumed exclusively by the coord
// package. See docs/adr/0002-scoped-holds.md for the composed
// closure/return-release model coord builds on top of these primitives.
//
// This package is internal and unexported: callers outside
// github.com/danmestas/bones must not depend on it.
package holds

import "errors"

// ErrHeldByAnother reports that Announce was called for a file already
// held by a different agent. Coord translates this into its own public
// sentinel when composing Claim.
var ErrHeldByAnother = errors.New("holds: file held by another agent")

// ErrClosed reports that a public method was called on a Manager whose
// Close has returned. Close-race with an in-flight call surfaces this
// error rather than a data race or nil dereference.
var ErrClosed = errors.New("holds: manager is closed")
