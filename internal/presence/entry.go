package presence

import (
	"encoding/json"
	"strings"
	"time"
)

// Entry is the value stored at each agent key in the KV bucket. The
// struct is persisted as JSON; every timestamp is wall-clock UTC so
// that two processes reading the same entry reach consistent verdicts.
type Entry struct {
	// AgentID identifies the agent the entry describes. It must be
	// non-empty per invariant 3 and matches Config.AgentID on the
	// owning Manager.
	AgentID string `json:"agent_id"`

	// Project scopes the entry. A Who call from project A filters out
	// entries whose Project is not A. Matches the coord.Config.AgentID
	// project-prefix scheme.
	Project string `json:"project"`

	// StartedAt is the UTC wall-clock time the owning Manager was
	// Opened. Immutable across heartbeats — a consumer watching
	// presence can tell "same agent, still up" from "agent restarted"
	// by comparing StartedAt across two reads.
	StartedAt time.Time `json:"started_at"`

	// LastSeen is refreshed on every heartbeat. Consumers computing
	// "is this entry fresh?" compare LastSeen against wall-clock now
	// plus the heartbeat cadence; TTL-based expiry also kicks the
	// entry out of the bucket, which is the final source of truth.
	LastSeen time.Time `json:"last_seen"`
}

// EventKind identifies the shape of a presence change delivered by
// Watch.
type EventKind int

const (
	// EventUp is delivered when a new entry appears in the bucket — a
	// fresh Put where the previous state was vacant, deleted, or
	// expired.
	EventUp EventKind = iota + 1

	// EventDown is delivered when an entry is removed from the bucket
	// — either an explicit Delete (clean shutdown) or a KV TTL
	// expiry (missed heartbeats).
	EventDown
)

// String returns a human-readable name for the EventKind.
func (k EventKind) String() string {
	switch k {
	case EventUp:
		return "Up"
	case EventDown:
		return "Down"
	default:
		return "Unknown"
	}
}

// Event is delivered to Watch callers on each observed presence change.
// AgentID and Project identify the subject; Kind is the change shape;
// Timestamp is the wall-clock moment the watcher observed the change.
type Event struct {
	AgentID   string
	Project   string
	Kind      EventKind
	Timestamp time.Time
}

// encode serializes an Entry to JSON bytes for KV storage.
func encode(e Entry) ([]byte, error) {
	return json.Marshal(e)
}

// decode deserializes an Entry from KV bytes. An error here means the
// bucket contains a corrupted entry, which the caller surfaces.
func decode(b []byte) (Entry, error) {
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// keyOf returns the KV key for (project, agentID). The key shape is
// "<project>/<agentID>" — '/' is in the KV key alphabet and doubles as
// a human-readable separator. Project-scoped scans use the
// "<project>/" prefix.
func keyOf(project, agentID string) string {
	return project + "/" + agentID
}

// splitKey parses a KV key back into (project, agentID). The split is
// on the first '/' — project segments cannot themselves contain '/' in
// the agent-infra naming scheme, so a single split is sufficient.
// Returns ("", "", false) on a malformed key (no '/').
func splitKey(key string) (string, string, bool) {
	idx := strings.IndexByte(key, '/')
	if idx < 0 {
		return "", "", false
	}
	return key[:idx], key[idx+1:], true
}
