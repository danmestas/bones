package swarm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EventsFile is the workspace-relative path of the structured
// swarm-lifecycle event log. Living next to the workspace leaf log
// keeps the audit-shaped data in one place; the dedicated filename
// makes the purpose obvious to operators tailing it.
//
// Path is `<workspace>/.bones/swarm-events.jsonl`. JSONL (one JSON
// object per line) is the right shape for `tail -f`, `grep`, and
// streaming-friendly tooling. Each line is small (~150 bytes) so
// POSIX-atomic appends from concurrent slot processes don't tear.
const EventsFile = ".bones/swarm-events.jsonl"

// EventKind enumerates the slot-lifecycle event types written to
// the JSONL log. Each kind corresponds to a distinct verb at the
// CLI surface; consumers (operator dashboards, replays) can filter
// by kind without parsing the rest of the line.
type EventKind string

const (
	// EventSlotJoin is emitted when Acquire successfully writes a
	// session record. The slot is bound to a task and a leaf is open.
	EventSlotJoin EventKind = "slot_join"

	// EventSlotCommit is emitted when ResumedLease.Commit returns
	// without a leaf-side error. PushErr / RenewErr surface in the
	// returned CommitResult but the structured event records the
	// successful local commit (audit-trail-relevant).
	EventSlotCommit EventKind = "slot_commit"

	// EventSlotClose is emitted when ResumedLease.Close completes
	// the cleanup path. Always carries Result so post-mortem
	// analysis sees success/fail/fork distinction.
	EventSlotClose EventKind = "slot_close"

	// EventSlotReap is emitted by the swarm reap verb instead of
	// EventSlotClose so operators can distinguish operator-driven
	// cleanup ("we shut this slot down") from substrate-driven
	// reaping ("the agent went silent and we cleaned up after it").
	EventSlotReap EventKind = "slot_reap"
)

// Event is the on-disk shape written to swarm-events.jsonl. Fields
// are minimal-but-sufficient for an operator to reconstruct who
// did what when. Optional fields use omitempty so close events
// don't carry stale CommitUUIDs and join events don't carry
// spurious Result strings.
type Event struct {
	TS         time.Time `json:"ts"`
	Kind       EventKind `json:"event"`
	Slot       string    `json:"slot"`
	TaskID     string    `json:"task_id,omitempty"`
	AgentID    string    `json:"agent_id,omitempty"`
	Host       string    `json:"host,omitempty"`
	Result     string    `json:"result,omitempty"`
	NoArtifact bool      `json:"no_artifact,omitempty"`
	CommitUUID string    `json:"commit_uuid,omitempty"`
}

// appendEvent appends one Event to the workspace-local JSONL log.
// Best-effort: a failed write is logged to stderr but never
// propagated as an error — losing an audit line is recoverable;
// failing the user-facing verb because the log is read-only is
// not. Append uses O_APPEND so concurrent writers don't clobber
// each other (POSIX guarantees atomic writes ≤PIPE_BUF for the
// small payloads we emit).
//
// workspaceDir is the workspace root (not .bones/). The file is
// created with 0o644 if missing; the .bones directory is assumed
// to exist (Acquire, the only caller path that can predate it,
// already ensures `<workspace>/.bones/swarm/<slot>` via its leaf
// open).
func appendEvent(workspaceDir string, e Event) {
	if workspaceDir == "" {
		return
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	path := filepath.Join(workspaceDir, EventsFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr,
			"swarm: append event: mkdir %s: %v\n", filepath.Dir(path), err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"swarm: append event: open %s: %v\n", path, err)
		return
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(e)
	if err != nil {
		fmt.Fprintf(os.Stderr, "swarm: append event: marshal: %v\n", err)
		return
	}
	b = append(b, '\n')
	if _, err := f.Write(b); err != nil {
		fmt.Fprintf(os.Stderr,
			"swarm: append event: write %s: %v\n", path, err)
	}
}

// emitJoinEvent is the call-site helper Acquire uses to keep its
// own body under the funlen cap. Pulls the os.Hostname() lookup
// and the appendEvent boilerplate out of the hot path.
func emitJoinEvent(workspaceDir, slot, taskID, fossilUser string, ts time.Time) {
	host, _ := os.Hostname()
	appendEvent(workspaceDir, Event{
		TS:      ts,
		Kind:    EventSlotJoin,
		Slot:    slot,
		TaskID:  taskID,
		AgentID: fossilUser,
		Host:    host,
	})
}
