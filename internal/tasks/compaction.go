package tasks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/bones/internal/assert"
)

// PerTaskEventCap is the maximum number of events kept per task before
// older events are compacted into a synthetic created summary. ADR
// 0052 fixes this at 50.
const PerTaskEventCap = 50

// EventRetentionWindow is the time-based fallback for compaction.
// Events older than this for a task that also has at least one newer
// event are eligible for compaction. ADR 0052 fixes this at 90 days.
const EventRetentionWindow = 90 * 24 * time.Hour

// CompactPerTask replaces the older events for one task with a single
// synthetic created event carrying the full post-compaction snapshot.
// Returns the number of events removed (i.e., per-task event count
// before minus after).
//
// CompactPerTask is the per-task primitive used by the hub-side
// compaction worker. The worker is a follow-up; this primitive lets
// tests exercise the compaction semantics without scheduling.
//
// Safety: compaction runs only on tasks whose latest projection
// exists in KV. Aborts (no-op) if the projection is missing — the
// task may have been purged.
func CompactPerTask(ctx context.Context, m *Manager, taskID string) (int, error) {
	assert.NotNil(ctx, "tasks.CompactPerTask: ctx is nil")
	assert.NotNil(m, "tasks.CompactPerTask: m is nil")
	assert.NotEmpty(taskID, "tasks.CompactPerTask: taskID is empty")
	if m.stream == nil {
		return 0, errors.New("tasks.CompactPerTask: event log disabled")
	}
	cur, _, err := m.Get(ctx, taskID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("tasks.CompactPerTask: %w", err)
	}
	envs, err := m.Replay(ctx, LogReadOpts{FilterTaskID: taskID})
	if err != nil {
		return 0, fmt.Errorf("tasks.CompactPerTask: replay: %w", err)
	}
	if len(envs) <= PerTaskEventCap {
		// Nothing to compact within the count cap; check the time
		// fallback instead.
		if !shouldCompactByTime(envs) {
			return 0, nil
		}
	}
	return doCompactPerTask(ctx, m, taskID, cur, envs)
}

// shouldCompactByTime reports whether the events slice has at least
// one event older than EventRetentionWindow paired with at least one
// event newer. The "paired with newer" rule keeps a frozen-but-active
// task from being compacted just because its events are old.
func shouldCompactByTime(envs []EventEnvelope) bool {
	if len(envs) == 0 {
		return false
	}
	cutoff := time.Now().UTC().Add(-EventRetentionWindow)
	hasOld := false
	hasNew := false
	for _, env := range envs {
		if env.Timestamp.Before(cutoff) {
			hasOld = true
		} else {
			hasNew = true
		}
		if hasOld && hasNew {
			return true
		}
	}
	return false
}

// doCompactPerTask emits a synthetic created event with the
// post-compaction snapshot and purges the per-task subject so the
// stream's per-task event count drops.
//
// Compaction order:
//
//  1. Publish synthetic created carrying the full Task snapshot.
//  2. Wait for ack so the survivor is durable.
//  3. Purge the per-task subject below the synthetic event's seq so
//     the older entries are reclaimed.
//
// Failure between steps 1 and 3 leaves a duplicate synthetic event
// in the log; readers (TaskTally, Replay) handle this gracefully —
// duplicate created events for the same task are idempotent.
func doCompactPerTask(
	ctx context.Context,
	m *Manager,
	taskID string,
	cur Task,
	envs []EventEnvelope,
) (int, error) {
	snapshot := cur
	payload := CreatedPayload{
		Title:    cur.Title,
		Files:    append([]string(nil), cur.Files...),
		Snapshot: &snapshot,
	}
	env, err := EncodeEnvelope(EventTypeCreated, taskID, payload)
	if err != nil {
		return 0, err
	}
	body, err := MarshalEnvelope(env)
	if err != nil {
		return 0, err
	}
	ack, err := m.js.Publish(ctx, EventSubject(taskID), body)
	if err != nil {
		return 0, fmt.Errorf("tasks.CompactPerTask: publish: %w", err)
	}
	// Purge per-task subject below the synthetic survivor.
	if err := m.stream.Purge(ctx, jetstream.WithPurgeSubject(EventSubject(taskID)),
		jetstream.WithPurgeKeep(1)); err != nil {
		return 0, fmt.Errorf("tasks.CompactPerTask: purge: %w", err)
	}
	_ = ack
	return len(envs), nil
}
