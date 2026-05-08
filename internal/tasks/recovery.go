package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/bones/internal/assert"
)

// migrationMarkerKey is the KV key whose presence (in the bones-tasks
// bucket) signals that the synthetic-event migration from pre-event-log
// state has already run. The marker is set at the end of a successful
// Migrate() call. Its value is a Task-shaped placeholder (see
// setMigrated) only because the bucket is typed for JSON Task records;
// presence is what counts.
const migrationMarkerKey = "__bones_tasks_events_migrated"

// Recover reconciles the KV projection with the event log on hub start.
// For each unique task ID with at least one event on the stream, the
// recovery loop walks events newer than the KV's LastEventSeq and
// applies them via AdminWrite (a no-op when the projection is already
// up to date). Returns the number of orphan events replayed.
//
// Recovery is idempotent: repeated calls on a stable substrate replay
// no events. Concurrent Recover calls race only on the underlying KV
// CAS; the substrate serializes them.
func Recover(ctx context.Context, m *Manager) (int, error) {
	assert.NotNil(ctx, "tasks.Recover: ctx is nil")
	assert.NotNil(m, "tasks.Recover: m is nil")
	if m.stream == nil {
		// No event log → nothing to recover.
		return 0, nil
	}
	tasksByID, err := snapshotTasks(ctx, m)
	if err != nil {
		return 0, fmt.Errorf("tasks.Recover: snapshot: %w", err)
	}
	cons, err := m.stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{AllEventsSubject},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return 0, fmt.Errorf("tasks.Recover: consumer: %w", err)
	}
	replayed, err := drainAndReplay(ctx, m, cons, tasksByID)
	if err != nil {
		return replayed, fmt.Errorf("tasks.Recover: replay: %w", err)
	}
	return replayed, nil
}

// snapshotTasks reads every current Task projection, indexed by ID, so
// the recovery loop can compare each event's stream sequence against
// the projection's stamped LastEventSeq without round-tripping the KV
// per event.
func snapshotTasks(ctx context.Context, m *Manager) (map[string]Task, error) {
	list, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Task, len(list))
	for _, t := range list {
		out[t.ID] = t
	}
	return out, nil
}

// drainAndReplay pulls every available event from cons, re-applies the
// ones whose effect has not yet landed in the KV projection, and
// returns the count of replayed events. Stops on the first ctx error
// or when the stream returns an end-of-data sentinel.
func drainAndReplay(
	ctx context.Context,
	m *Manager,
	cons jetstream.Consumer,
	tasksByID map[string]Task,
) (int, error) {
	const drainBatch = 200
	const drainWait = 250 * time.Millisecond
	replayed := 0
	for {
		batch, err := cons.Fetch(drainBatch, jetstream.FetchMaxWait(drainWait))
		if err != nil {
			return replayed, err
		}
		empty := true
		for msg := range batch.Messages() {
			empty = false
			env, perr := UnmarshalEnvelope(msg.Data())
			if perr != nil {
				_ = msg.Ack()
				continue
			}
			meta, _ := msg.Metadata()
			if meta != nil {
				env.StreamSeq = meta.Sequence.Stream
			}
			cur, hasProjection := tasksByID[env.TaskID]
			if hasProjection && cur.LastEventSeq >= env.StreamSeq {
				_ = msg.Ack()
				continue
			}
			if err := applyEvent(ctx, m, env); err != nil {
				_ = msg.Ack()
				return replayed, err
			}
			replayed++
			_ = msg.Ack()
			if t, _, gerr := m.Get(ctx, env.TaskID); gerr == nil {
				tasksByID[env.TaskID] = t
			}
		}
		if empty {
			return replayed, nil
		}
		if err := batch.Error(); err != nil {
			return replayed, err
		}
	}
}

// applyEvent re-applies env to the KV projection via AdminWrite. The
// per-type projection logic lives here so all replay paths (recovery
// and TaskTally) share one code path. Unknown event types are skipped
// (additive-only evolution rule per ADR 0052).
func applyEvent(ctx context.Context, m *Manager, env EventEnvelope) error {
	aw := NewAdminWrite(m)
	switch env.Type {
	case EventTypeCreated:
		return applyCreatedEvent(ctx, aw, env)
	case EventTypeClaimed:
		return applyClaimedEvent(ctx, aw, env)
	case EventTypeUnclaimed:
		return applyUnclaimedEvent(ctx, aw, env)
	case EventTypeUpdated:
		return applyUpdatedEvent(ctx, aw, env)
	case EventTypeLinked:
		return applyLinkedEvent(ctx, aw, env)
	case EventTypeSlotChanged:
		// slot_changed has no KV projection; only LastEventSeq advances.
		return aw.Update(ctx, env.TaskID, func(t Task) (Task, error) {
			t.LastEventSeq = env.StreamSeq
			return t, nil
		})
	case EventTypeClosed:
		return applyClosedEvent(ctx, aw, env)
	default:
		// Unknown type — additive-only evolution; leave projection alone.
		return nil
	}
}

func applyCreatedEvent(ctx context.Context, aw AdminWrite, env EventEnvelope) error {
	p, err := DecodePayload(env)
	if err != nil {
		return err
	}
	created := p.(CreatedPayload)
	if created.Snapshot != nil {
		// Migration / compaction synthetic: write the full snapshot.
		snap := *created.Snapshot
		snap.LastEventSeq = env.StreamSeq
		err := aw.Create(ctx, snap)
		if errors.Is(err, ErrAlreadyExists) {
			// Project the snapshot via Update.
			return aw.Update(ctx, env.TaskID, func(t Task) (Task, error) {
				snap.LastEventSeq = env.StreamSeq
				return snap, nil
			})
		}
		return err
	}
	rec := Task{
		ID:            env.TaskID,
		Title:         created.Title,
		Status:        StatusOpen,
		Files:         append([]string(nil), created.Files...),
		CreatedAt:     env.Timestamp,
		UpdatedAt:     env.Timestamp,
		SchemaVersion: SchemaVersion,
		LastEventSeq:  env.StreamSeq,
	}
	err = aw.Create(ctx, rec)
	if errors.Is(err, ErrAlreadyExists) {
		return nil
	}
	return err
}

func applyClaimedEvent(ctx context.Context, aw AdminWrite, env EventEnvelope) error {
	p, err := DecodePayload(env)
	if err != nil {
		return err
	}
	cl := p.(ClaimedPayload)
	return aw.Update(ctx, env.TaskID, func(t Task) (Task, error) {
		// Recovery applies idempotently. If projection is already past
		// this event, the LastEventSeq guard above handled it.
		t.Status = StatusClaimed
		t.ClaimedBy = cl.AgentID
		if cl.ClaimEpoch > t.ClaimEpoch {
			t.ClaimEpoch = cl.ClaimEpoch
		}
		t.UpdatedAt = env.Timestamp
		t.LastEventSeq = env.StreamSeq
		return t, nil
	})
}

func applyUnclaimedEvent(ctx context.Context, aw AdminWrite, env EventEnvelope) error {
	return aw.Update(ctx, env.TaskID, func(t Task) (Task, error) {
		t.Status = StatusOpen
		t.ClaimedBy = ""
		t.UpdatedAt = env.Timestamp
		t.LastEventSeq = env.StreamSeq
		return t, nil
	})
}

func applyLinkedEvent(ctx context.Context, aw AdminWrite, env EventEnvelope) error {
	p, err := DecodePayload(env)
	if err != nil {
		return err
	}
	lp := p.(LinkedPayload)
	return aw.Update(ctx, env.TaskID, func(t Task) (Task, error) {
		for _, e := range t.Edges {
			if e.Type == EdgeType(lp.EdgeType) && e.Target == lp.OtherID {
				t.LastEventSeq = env.StreamSeq
				return t, nil
			}
		}
		t.Edges = append(t.Edges, Edge{
			Type:   EdgeType(lp.EdgeType),
			Target: lp.OtherID,
		})
		t.UpdatedAt = env.Timestamp
		t.LastEventSeq = env.StreamSeq
		return t, nil
	})
}

// applyUpdatedEvent walks the FieldChange tuples on env and applies
// each one's New JSON blob to the corresponding Task field. Per ADR
// 0052 §"Recovery": "the log alone — no KV consult — must suffice to
// reconstruct any prior state." The (field, old, new) shape is what
// makes that possible; this dispatcher is what consumes it.
//
// Unknown field names are skipped (additive-only evolution rule); a
// future schema addition can grow this dispatcher without breaking
// older logs.
func applyUpdatedEvent(ctx context.Context, aw AdminWrite, env EventEnvelope) error {
	p, err := DecodePayload(env)
	if err != nil {
		return err
	}
	up := p.(UpdatedPayload)
	return aw.Update(ctx, env.TaskID, func(t Task) (Task, error) {
		for _, ch := range up.Changes {
			if applyErr := applyFieldChange(&t, ch); applyErr != nil {
				return t, fmt.Errorf("applyUpdatedEvent: field %q: %w",
					ch.Field, applyErr)
			}
		}
		t.UpdatedAt = env.Timestamp
		t.LastEventSeq = env.StreamSeq
		return t, nil
	})
}

// applyFieldChange unmarshals ch.New into the corresponding field on
// t. The dispatcher is keyed by ch.Field; field names are the stable
// JSON tag values (e.g. "title", "claimed_by"), not Go struct names.
// Unknown fields are silently dropped to honor the additive-only rule.
//
// Adding a new task field:
//
//  1. Add the field to Task with a json tag.
//  2. Update diffTaskFields in cli/tasks_update.go to emit a tuple
//     when the field changes.
//  3. Add a case here to apply the unmarshal.
//  4. Add a test under TestRecover_AppliesUpdatedEventField_<field>.
func applyFieldChange(t *Task, ch FieldChange) error {
	switch ch.Field {
	case "title":
		return jsonInto(ch.New, &t.Title)
	case "status":
		return jsonInto(ch.New, &t.Status)
	case "claimed_by":
		return jsonInto(ch.New, &t.ClaimedBy)
	case "parent":
		return jsonInto(ch.New, &t.Parent)
	case "files":
		return jsonInto(ch.New, &t.Files)
	case "context":
		return jsonInto(ch.New, &t.Context)
	case "defer_until":
		return jsonInto(ch.New, &t.DeferUntil)
	case "edges":
		return jsonInto(ch.New, &t.Edges)
	case "closed_by":
		return jsonInto(ch.New, &t.ClosedBy)
	case "closed_reason":
		return jsonInto(ch.New, &t.ClosedReason)
	case "claim_epoch":
		return jsonInto(ch.New, &t.ClaimEpoch)
	default:
		// Unknown field — additive-only rule, drop quietly.
		return nil
	}
}

// jsonInto is a tiny helper that unmarshals raw into target. Pulled
// out so each case in applyFieldChange stays a single line.
func jsonInto(raw json.RawMessage, target any) error {
	return json.Unmarshal(raw, target)
}

func applyClosedEvent(ctx context.Context, aw AdminWrite, env EventEnvelope) error {
	p, err := DecodePayload(env)
	if err != nil {
		return err
	}
	cp := p.(ClosedPayload)
	return aw.Update(ctx, env.TaskID, func(t Task) (Task, error) {
		// Idempotent path: projection is already closed. Bumping
		// LastEventSeq alone is allowed under the closed→closed
		// compaction-only rule (LastEventSeq is not in the
		// eqNonCompaction check, so it counts as a compaction-style
		// field change).
		if t.Status == StatusClosed {
			t.LastEventSeq = env.StreamSeq
			return t, nil
		}
		t.Status = StatusClosed
		closer := cp.AgentID
		if closer == "" && t.ClaimedBy != "" {
			closer = t.ClaimedBy
		}
		t.ClosedBy = closer
		t.ClosedReason = cp.Reason
		ts := env.Timestamp
		t.ClosedAt = &ts
		t.ClaimedBy = ""
		t.UpdatedAt = ts
		t.LastEventSeq = env.StreamSeq
		return t, nil
	})
}

// Migrate emits synthetic events for every pre-existing KV record so
// the event log carries a baseline for recovery and audit. Idempotent
// via migrationMarkerKey; running Migrate twice is a no-op.
//
// This is the one-time path for upgrading a workspace from before the
// event log was the source of truth (schema v3 or earlier). Documented
// limit per ADR 0052: pre-migration update history is lost — only
// created / claimed / closed bookmarks are reconstructible.
func Migrate(ctx context.Context, m *Manager) (int, error) {
	assert.NotNil(ctx, "tasks.Migrate: ctx is nil")
	assert.NotNil(m, "tasks.Migrate: m is nil")
	if m.stream == nil {
		return 0, errors.New("tasks.Migrate: event log disabled")
	}
	if migrated, err := isMigrated(ctx, m); err != nil {
		return 0, fmt.Errorf("tasks.Migrate: marker: %w", err)
	} else if migrated {
		return 0, nil
	}
	list, err := m.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("tasks.Migrate: list: %w", err)
	}
	emitted, err := emitSyntheticEvents(ctx, m, list)
	if err != nil {
		return emitted, fmt.Errorf("tasks.Migrate: emit: %w", err)
	}
	if err := setMigrated(ctx, m); err != nil {
		return emitted, fmt.Errorf("tasks.Migrate: set marker: %w", err)
	}
	return emitted, nil
}

// emitSyntheticEvents publishes one created event per pre-existing
// task, plus a claimed event when ClaimedBy is non-empty, plus a
// closed event when ClosedAt is non-nil. Each created event embeds a
// snapshot of the projection so a fresh recovery can rebuild the KV
// from the log alone. After publishing each task's events, the
// projection's LastEventSeq is stamped to the highest sequence
// emitted so subsequent Recover runs do not double-apply the same
// synthetic events.
func emitSyntheticEvents(
	ctx context.Context, m *Manager, list []Task,
) (int, error) {
	emitted := 0
	aw := NewAdminWrite(m)
	for _, t := range list {
		if strings.HasPrefix(t.ID, "__bones_tasks_events_migrated") {
			// Skip the marker key if it ever appears in List output.
			continue
		}
		var lastSeq uint64
		// created — embeds full snapshot for log-only reconstruction.
		snap := t
		snap.SchemaVersion = SchemaVersion
		createdPayload := CreatedPayload{
			Title:    t.Title,
			Files:    append([]string(nil), t.Files...),
			Snapshot: &snap,
		}
		env, err := EncodeEnvelope(EventTypeCreated, t.ID, createdPayload)
		if err != nil {
			return emitted, err
		}
		env.Timestamp = t.CreatedAt
		seq, err := publishMigrationEnvelope(ctx, m, env)
		if err != nil {
			return emitted, err
		}
		lastSeq = seq
		emitted++
		if t.ClaimedBy != "" {
			env, err := EncodeEnvelope(EventTypeClaimed, t.ID, ClaimedPayload{
				AgentID:    t.ClaimedBy,
				ClaimEpoch: t.ClaimEpoch,
			})
			if err != nil {
				return emitted, err
			}
			env.Timestamp = t.UpdatedAt
			seq, err := publishMigrationEnvelope(ctx, m, env)
			if err != nil {
				return emitted, err
			}
			lastSeq = seq
			emitted++
		}
		if t.ClosedAt != nil {
			env, err := EncodeEnvelope(EventTypeClosed, t.ID, ClosedPayload{
				AgentID: t.ClosedBy,
				Reason:  t.ClosedReason,
			})
			if err != nil {
				return emitted, err
			}
			env.Timestamp = *t.ClosedAt
			seq, err := publishMigrationEnvelope(ctx, m, env)
			if err != nil {
				return emitted, err
			}
			lastSeq = seq
			emitted++
		}
		// Stamp LastEventSeq on the KV record so Recover does not
		// re-apply these synthetic events.
		if err := aw.Update(ctx, t.ID, func(cur Task) (Task, error) {
			cur.LastEventSeq = lastSeq
			return cur, nil
		}); err != nil {
			return emitted, err
		}
	}
	return emitted, nil
}

// publishMigrationEnvelope publishes env on the events stream, used
// only by Migrate. Returns the assigned stream sequence.
func publishMigrationEnvelope(
	ctx context.Context, m *Manager, env EventEnvelope,
) (uint64, error) {
	body, err := MarshalEnvelope(env)
	if err != nil {
		return 0, err
	}
	ack, err := m.js.Publish(ctx, EventSubject(env.TaskID), body)
	if err != nil {
		return 0, err
	}
	return ack.Sequence, nil
}

// isMigrated reports whether the migration marker is present in the
// task KV bucket.
func isMigrated(ctx context.Context, m *Manager) (bool, error) {
	_, err := m.kv.Get(ctx, migrationMarkerKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// setMigrated stamps the migration marker so subsequent Migrate calls
// short-circuit. Encodes a Task-shaped placeholder so the bucket's
// JSON expectations stay satisfied.
func setMigrated(ctx context.Context, m *Manager) error {
	now := time.Now().UTC()
	// Use a Task envelope to satisfy the KV's value-shape expectations.
	marker := Task{
		ID:            migrationMarkerKey,
		Title:         "tasks-events migration marker",
		Status:        StatusClosed,
		ClosedAt:      &now,
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: SchemaVersion,
	}
	payload, err := encode(marker)
	if err != nil {
		return err
	}
	_, err = m.kv.Put(ctx, migrationMarkerKey, payload)
	return err
}
