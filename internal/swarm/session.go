// Package swarm holds the per-slot session record schema and the
// JetStream-KV-backed Manager that bones swarm verbs use to track
// active swarm sessions in a workspace.
//
// State lives in a single KV bucket (DefaultBucketName) keyed by slot
// name. ADR 0028 §"Lifecycle and state" explains why session state
// went to KV instead of a per-slot state.json on disk: every field
// in this struct is either already authoritative somewhere else in
// JetStream KV (task_id in bones-tasks, agent_id in bones-presence,
// the claim hold in bones-holds) or describes the host-local OS
// process owning the leaf (host, leaf_pid). Putting them in one
// bucket gives cross-host visibility and a single source of truth
// for `bones swarm status` and `bones doctor` without re-deriving
// state from N substrate buckets per call.
//
// The Manager is intentionally simpler than internal/presence: there
// is no background heartbeat goroutine. Heartbeats are driven by
// agent-process calls to `bones swarm commit`, which extends TTL via
// CAS. A slot whose agent has crashed stops heartbeating and the
// bucket TTL evicts the entry — exactly the surface `bones doctor`
// is meant to see.
package swarm

import (
	"encoding/json"
	"fmt"
	"time"
)

// Session is the per-slot record persisted in the bones-swarm-sessions
// JetStream KV bucket. One record per active slot in a workspace.
// Slot names are workspace-scoped because the bucket itself is per-
// NATS-deployment.
//
// Marshaled as JSON for human-readable debugging via `nats kv get`.
// All time fields are RFC3339-encoded UTC. New fields must default
// safely on a missing-key read so older clients can read newer
// records (additive evolution only).
type Session struct {
	// Slot is the slot name (matches the plan's `[slot: X]` annotation).
	// Doubles as the KV bucket key — slot is the primary identifier.
	Slot string `json:"slot"`

	// TaskID identifies the task this slot is currently working on.
	// Lookup against bones-tasks for full task state; this field is a
	// denormalized pointer used by `bones swarm status` and the close
	// verb so they don't need a second lookup.
	TaskID string `json:"task_id"`

	// AgentID is "slot-<name>" — the slot's fossil + coord identity.
	// Matches the agent_id used for the holds and presence buckets so
	// claim ownership and liveness join cleanly across substrates.
	AgentID string `json:"agent_id"`

	// Host is os.Hostname() of the machine running the leaf. Different
	// from the workspace host means another machine owns this slot;
	// `bones swarm` verbs abort with a clear cross-host error rather
	// than try to manage a remote leaf process.
	Host string `json:"host"`

	// LeafPID is the host-local PID of the leaf process. Only
	// meaningful on the matching Host. Probed via os.FindProcess +
	// signal 0 to detect crashes.
	LeafPID int `json:"leaf_pid"`

	// StartedAt is when this session record was first written.
	// Immutable once set; later renewals only touch LastRenewed.
	StartedAt time.Time `json:"started_at"`

	// LastRenewed is updated on every `swarm commit` (which heartbeats
	// the session) and used by `bones doctor` to flag stale entries.
	LastRenewed time.Time `json:"last_renewed"`
}

// encode serializes a Session into a JSON byte slice for KV storage.
// JSON keeps the bucket browsable from `nats kv` CLI tools without
// needing custom decoders for ad-hoc operator inspection.
func encode(s Session) ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("swarm: encode session: %w", err)
	}
	return b, nil
}

// decode parses a JSON-encoded Session out of a KV value.
func decode(b []byte) (Session, error) {
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return Session{}, fmt.Errorf("swarm: decode session: %w", err)
	}
	return s, nil
}
