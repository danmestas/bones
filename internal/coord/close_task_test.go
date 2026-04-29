package coord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
)

// seedTaskID is a shape-valid TaskID used by every close test. Tests
// that need a second ID derive from this one so the fixture stays
// obvious at the call site. ADR 0005 pins the shape to
// <proj>-<8lowalnum>; the value here passes that predicate.
const seedTaskID = "bones-c46close"

// seedClaimedTask writes a claimed task record bound to the Coord's
// configured AgentID. The record is created directly via c.sub.tasks.Create
// so the test is same-package; that path sits below invariant 13's
// transition DAG (Create has no prior state to transition from) but
// still runs invariant 11's claimed_by/status coupling check, so the
// combination of status=claimed + claimed_by=agent is accepted.
func seedClaimedTask(t *testing.T, c *Coord, id string) {
	t.Helper()
	now := time.Now().UTC()
	rec := tasks.Task{
		ID:            id,
		Title:         "seed claimed",
		Status:        tasks.StatusClaimed,
		ClaimedBy:     c.cfg.AgentID,
		Files:         []string{"/seed/a.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: tasks.SchemaVersion,
	}
	if err := c.sub.tasks.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed claimed task: %v", err)
	}
}

// seedOpenTask writes an open, unclaimed task record.
func seedOpenTask(t *testing.T, c *Coord, id string) {
	t.Helper()
	now := time.Now().UTC()
	rec := tasks.Task{
		ID:            id,
		Title:         "seed open",
		Status:        tasks.StatusOpen,
		Files:         []string{"/seed/a.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: tasks.SchemaVersion,
	}
	if err := c.sub.tasks.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed open task: %v", err)
	}
}

// seedClaimedByOther writes a claimed task owned by a peer agent.
func seedClaimedByOther(
	t *testing.T, c *Coord, id, peer string,
) {
	t.Helper()
	now := time.Now().UTC()
	rec := tasks.Task{
		ID:            id,
		Title:         "seed other",
		Status:        tasks.StatusClaimed,
		ClaimedBy:     peer,
		Files:         []string{"/seed/a.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: tasks.SchemaVersion,
	}
	if err := c.sub.tasks.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed peer-claimed task: %v", err)
	}
}

// seedClosedTask writes a terminal (closed) task record. ClaimedBy is
// empty per invariant 11.
func seedClosedTask(t *testing.T, c *Coord, id string) {
	t.Helper()
	now := time.Now().UTC()
	rec := tasks.Task{
		ID:            id,
		Title:         "seed closed",
		Status:        tasks.StatusClosed,
		Files:         []string{"/seed/a.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		ClosedAt:      &now,
		ClosedBy:      "agent-prior",
		ClosedReason:  "prior close",
		SchemaVersion: tasks.SchemaVersion,
	}
	if err := c.sub.tasks.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed closed task: %v", err)
	}
}

// TestCloseTask_HappyPath covers the claimed→closed edge on a task
// owned by the calling agent: CloseTask succeeds, and the persisted
// record reflects every close field (status, closed_by, closed_at,
// closed_reason) with the agent-supplied reason preserved verbatim.
func TestCloseTask_HappyPath(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	seedClaimedTask(t, c, seedTaskID)

	reason := "work done"
	before := time.Now().UTC()
	err := c.CloseTask(
		context.Background(), TaskID(seedTaskID), reason,
	)
	if err != nil {
		t.Fatalf("CloseTask: unexpected error: %v", err)
	}
	after := time.Now().UTC()

	got, _, err := c.sub.tasks.Get(context.Background(), seedTaskID)
	if err != nil {
		t.Fatalf("Get post-close: %v", err)
	}
	if got.Status != tasks.StatusClosed {
		t.Fatalf("Status: got %q, want closed", got.Status)
	}
	if got.ClosedBy != c.cfg.AgentID {
		t.Fatalf(
			"ClosedBy: got %q, want %q", got.ClosedBy, c.cfg.AgentID,
		)
	}
	if got.ClosedReason != reason {
		t.Fatalf(
			"ClosedReason: got %q, want %q", got.ClosedReason, reason,
		)
	}
	if got.ClosedAt == nil {
		t.Fatalf("ClosedAt: got nil, want non-nil")
	}
	if got.ClosedAt.Before(before) || got.ClosedAt.After(after) {
		t.Fatalf(
			"ClosedAt %v outside [%v, %v]",
			got.ClosedAt, before, after,
		)
	}
	if got.ClaimedBy != "" {
		t.Fatalf(
			"ClaimedBy: got %q, want empty (invariant 11)",
			got.ClaimedBy,
		)
	}
}

// TestCloseTask_OpenTask_ReturnsAgentMismatch covers the
// no-admin-override rule: an open (unclaimed) task has ClaimedBy == ""
// per invariant 11, and the identity check treats the empty string as
// a mismatch against the caller's AgentID.
func TestCloseTask_OpenTask_ReturnsAgentMismatch(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	seedOpenTask(t, c, seedTaskID)

	err := c.CloseTask(
		context.Background(), TaskID(seedTaskID), "why",
	)
	if !errors.Is(err, ErrAgentMismatch) {
		t.Fatalf("CloseTask: got %v, want ErrAgentMismatch", err)
	}

	got, _, gerr := c.sub.tasks.Get(context.Background(), seedTaskID)
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if got.Status != tasks.StatusOpen {
		t.Fatalf(
			"Status unchanged expected open, got %q", got.Status,
		)
	}
}

// TestCloseTask_ClaimedByOther_ReturnsAgentMismatch proves the
// closer-identity invariant: another agent holds the claim, so the
// calling agent's CloseTask is rejected without mutation.
func TestCloseTask_ClaimedByOther_ReturnsAgentMismatch(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	seedClaimedByOther(t, c, seedTaskID, "other-agent")

	err := c.CloseTask(
		context.Background(), TaskID(seedTaskID), "why",
	)
	if !errors.Is(err, ErrAgentMismatch) {
		t.Fatalf("CloseTask: got %v, want ErrAgentMismatch", err)
	}

	got, _, gerr := c.sub.tasks.Get(context.Background(), seedTaskID)
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if got.Status != tasks.StatusClaimed {
		t.Fatalf("Status: got %q, want claimed", got.Status)
	}
	if got.ClaimedBy != "other-agent" {
		t.Fatalf(
			"ClaimedBy: got %q, want other-agent", got.ClaimedBy,
		)
	}
}

// TestCloseTask_NotFound returns ErrTaskNotFound when no record exists
// at the given TaskID. The bucket is not mutated.
func TestCloseTask_NotFound(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()

	err := c.CloseTask(
		context.Background(), TaskID("bones-ghost001"), "why",
	)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("CloseTask: got %v, want ErrTaskNotFound", err)
	}
}

// TestCloseTask_AlreadyClosed returns ErrTaskAlreadyClosed and leaves
// the persisted record unchanged — invariant 13 makes closed terminal.
func TestCloseTask_AlreadyClosed(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	seedClosedTask(t, c, seedTaskID)

	err := c.CloseTask(
		context.Background(), TaskID(seedTaskID), "why",
	)
	if !errors.Is(err, ErrTaskAlreadyClosed) {
		t.Fatalf(
			"CloseTask: got %v, want ErrTaskAlreadyClosed", err,
		)
	}

	got, _, gerr := c.sub.tasks.Get(context.Background(), seedTaskID)
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if got.ClosedReason != "prior close" {
		t.Fatalf(
			"ClosedReason mutated: got %q, want prior close",
			got.ClosedReason,
		)
	}
	if got.ClosedBy != "agent-prior" {
		t.Fatalf(
			"ClosedBy mutated: got %q, want agent-prior",
			got.ClosedBy,
		)
	}
}

// TestCloseTask_StaleEpoch_Refused verifies Invariant 24: CloseTask
// refuses a write when the record's ClaimEpoch has been bumped past the
// caller's view in activeEpochs (simulating a concurrent Reclaim by a
// peer). ErrEpochStale must be returned and the record must remain
// unchanged. ADR 0007.
func TestCloseTask_StaleEpoch_Refused(t *testing.T) {
	// Simulate: A claims. A's Coord remembers epoch=1. Then the KV
	// record's ClaimEpoch gets bumped out from under A (as if B had
	// Reclaimed — we don't need the real Reclaim yet, la2.4 adds it).
	// A's CloseTask must return ErrEpochStale.
	c := newTestCoord(t, "agent-1")
	ctx := context.Background()

	taskID, rel := openAndClaim(t, c, "stale epoch task", []string{"/a.go"})
	defer func() { _ = rel() }()

	// Bump the record's epoch directly via the substrate to simulate
	// a concurrent Reclaim having bumped past our view.
	err := c.sub.tasks.Update(ctx, string(taskID), func(cur tasks.Task) (tasks.Task, error) {
		cur.ClaimEpoch += 1
		return cur, nil
	})
	if err != nil {
		t.Fatalf("simulated bump: %v", err)
	}

	err = c.CloseTask(ctx, taskID, "done")
	if !errors.Is(err, ErrEpochStale) {
		t.Fatalf("want ErrEpochStale, got %v", err)
	}
}

// TestCloseTask_NoTracker_EpochNonZero_Refused verifies the symmetric
// fence: when activeEpochs has no entry for the task (simulates a Coord
// that was restarted after Claim — in-memory tracker gone, KV record
// still has ClaimEpoch > 0), CloseTask must treat the expected epoch as
// zero and refuse the write. Matches checkEpoch in Commit. ADR 0007.
func TestCloseTask_NoTracker_EpochNonZero_Refused(t *testing.T) {
	// Simulates post-restart: task is in KV with ClaimEpoch > 0 and
	// ClaimedBy=agent, but activeEpochs is empty (crashed/restarted
	// Coord). CloseTask must refuse.
	c := newTestCoord(t, "agent-1")
	ctx := context.Background()

	taskID, rel := openAndClaim(t, c, "no tracker task", []string{"/a.go"})
	defer func() { _ = rel() }()

	// Simulate restart by clearing the in-memory tracker.
	c.activeEpochs.Delete(taskID)

	err := c.CloseTask(ctx, taskID, "done")
	if !errors.Is(err, ErrEpochStale) {
		t.Fatalf("want ErrEpochStale, got %v", err)
	}
}

// TestCloseTask_InvariantPanics covers the three assert-panic
// preconditions: nil ctx, empty TaskID, and use-after-close. Every case
// is a programmer error at the caller and must abort via panic rather
// than return a sentinel (see docs/invariants.md, invariants 1, 2, 8).
func TestCloseTask_InvariantPanics(t *testing.T) {
	t.Run("nil ctx", func(t *testing.T) {
		c := mustOpen(t)
		defer func() { _ = c.Close() }()
		requirePanic(t, func() {
			_ = c.CloseTask(nilCtx, TaskID("bones-nilctx01"), "r")
		}, "ctx is nil")
	})
	t.Run("empty taskID", func(t *testing.T) {
		c := mustOpen(t)
		defer func() { _ = c.Close() }()
		requirePanic(t, func() {
			_ = c.CloseTask(context.Background(), TaskID(""), "r")
		}, "taskID is empty")
	})
	t.Run("use after close", func(t *testing.T) {
		c := mustOpen(t)
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		requirePanic(t, func() {
			_ = c.CloseTask(
				context.Background(),
				TaskID("bones-uac00001"),
				"r",
			)
		}, "coord is closed")
	})
}
