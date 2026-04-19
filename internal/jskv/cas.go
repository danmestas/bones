// Package jskv holds JetStream KV primitives shared across the
// CAS-backed substrate packages (internal/holds, internal/tasks, and
// any future Phase 4 consumer — presence, subscriber registry). The
// package stays deliberately tiny: its only job is to prevent the
// same ten-line conflict predicate and the same eight-retry bound
// from drifting between call sites. A third duplicate is the point
// at which this extraction earns its weight (see agent-infra-5o0).
package jskv

import (
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

// MaxRetries caps any read-decide-write CAS loop built on JetStream
// KV. Each retry costs one KV Get plus one conditional write, and
// every loss means another caller advanced the revision — so the
// bound is really "how much concurrent churn on a single key before
// we surrender." Eight is generous: even pathological contention
// should converge in two or three rounds in practice, and the hard
// cap keeps a stuck loop from stalling its caller indefinitely. A
// bounded loop is TigerStyle; this is the bound.
const MaxRetries = 8

// IsConflict reports whether err is a JetStream KV revision-guard
// rejection — either a Create on a key that already exists or an
// Update whose expected-last-sequence did not match the current
// sequence. Both surface as the server API error code
// JSErrCodeStreamWrongLastSequence (10071); ErrKeyExists carries
// that same code, so errors.Is covers the Update path too via the
// jsError/APIError unwrap chain. We additionally compare the raw
// APIError code so a future library change that ungroups the two
// sentinels won't silently turn CAS conflicts into "unknown error".
func IsConflict(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, jetstream.ErrKeyExists) {
		return true
	}
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		if apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence {
			return true
		}
	}
	return false
}
