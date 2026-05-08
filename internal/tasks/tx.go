package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/assert"
)

// ErrNoMutations reports that a Tx callback returned without invoking
// any tx.X method. Tx is the only mutation entry point on Manager and
// returning without one is a programmer error caught at the API
// boundary rather than allowed to silently no-op.
var ErrNoMutations = errors.New(
	"tasks: Tx callback made no mutations",
)

// ErrEventLogDisabled reports that a Tx mutation was attempted on a
// Manager opened without an event log. coord's archive Manager opens
// in this mode for compaction-only writes; production Tx callers
// always have the event log enabled.
var ErrEventLogDisabled = errors.New(
	"tasks: event log disabled on this manager",
)

// Tx is the carrier passed to Manager.Tx's callback. Each tx.X method
// is one mutation. Tx is short-lived: its scope is the duration of one
// Manager.Tx call. Methods are not safe to call after the callback
// returns.
//
// Tx is not concurrency-safe. The callback must serialize its tx.X
// calls; callers needing multiple parallel writes should call Manager.Tx
// once per task and let the substrate serialize.
type Tx struct {
	mgr    *Manager
	ctx    context.Context
	taskID string

	// dirty is true once at least one tx.X method has been invoked. Tx
	// returns ErrNoMutations when the callback returns with dirty == false.
	dirty bool

	// firstErr is the first non-nil error from any tx.X method. The
	// callback may continue calling tx.X methods after an error; only
	// the first one is reported to the Tx caller.
	firstErr error
}

// Tx is the only public mutation entry point on Manager. The callback
// receives a *Tx and calls one or more tx.X methods to mutate the
// task. Each tx.X method publishes one event to the JetStream event
// log, then writes the corresponding KV projection. Returns
// ErrNoMutations if the callback completes without calling any tx.X
// method, or any error returned by the callback or any tx.X call.
//
// Concurrency: multiple Tx invocations on the same task race; the KV
// CAS layer detects the conflict and the losing call retries inside
// the underlying jskv.MaxRetries-bounded loop.
func (m *Manager) Tx(
	ctx context.Context, taskID string,
	fn func(tx *Tx) error,
) error {
	assert.NotNil(ctx, "tasks.Tx: ctx is nil")
	assert.NotEmpty(taskID, "tasks.Tx: taskID is empty")
	assert.NotNil(fn, "tasks.Tx: fn is nil")
	if m.done.Load() {
		return ErrClosed
	}
	tx := &Tx{mgr: m, ctx: ctx, taskID: taskID}
	if err := fn(tx); err != nil {
		return err
	}
	if tx.firstErr != nil {
		return tx.firstErr
	}
	if !tx.dirty {
		return ErrNoMutations
	}
	return nil
}

// markDirty flags the Tx as having attempted at least one mutation.
// Called at the top of every tx.X method.
func (tx *Tx) markDirty() { tx.dirty = true }

// recordErr records err as the Tx's first error if no error has yet
// been recorded. Subsequent tx.X calls in the same callback observe
// the recorded error but may still execute (the callback decides
// whether to short-circuit; the substrate will not commit further
// changes if an earlier publish failed durably).
func (tx *Tx) recordErr(err error) {
	if tx.firstErr == nil {
		tx.firstErr = err
	}
}

// Create publishes a created event and writes the initial KV record.
// t.ID must equal tx's taskID. The event Snapshot field is left
// unpopulated; readers reconstruct the projection from the live KV.
func (tx *Tx) Create(t Task) error {
	tx.markDirty()
	if t.ID != tx.taskID {
		err := fmt.Errorf("tasks.Tx.Create: t.ID %q != tx.taskID %q",
			t.ID, tx.taskID)
		tx.recordErr(err)
		return err
	}
	if err := validateForCreate(t); err != nil {
		tx.recordErr(err)
		return err
	}
	payload := CreatedPayload{
		Title: t.Title,
		Files: append([]string(nil), t.Files...),
	}
	env, err := EncodeEnvelope(EventTypeCreated, t.ID, payload)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	seq, err := tx.publish(env)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	t.LastEventSeq = seq
	if err := tx.mgr.create(tx.ctx, t); err != nil {
		tx.recordErr(err)
		return err
	}
	return nil
}

// Claim publishes a claimed event and updates the KV record's status
// and ClaimedBy fields. The mutator closure stays the same shape used
// by the legacy Update API; tx.Claim wraps it so the caller does not
// hand-author one. The claimEpoch is the new epoch the caller wants
// stamped on the record (typically prior + 1, computed by coord).
//
// The optional mutate callback runs inside the CAS loop AFTER the
// status/agent fields are stamped, so callers (coord) can apply
// extra fields like ClaimEpoch in the same revision.
func (tx *Tx) Claim(agentID string, slot string, claimEpoch uint64) error {
	tx.markDirty()
	if agentID == "" {
		err := errors.New("tasks.Tx.Claim: agentID is empty")
		tx.recordErr(err)
		return err
	}
	payload := ClaimedPayload{
		AgentID:    agentID,
		Slot:       slot,
		ClaimEpoch: claimEpoch,
	}
	env, err := EncodeEnvelope(EventTypeClaimed, tx.taskID, payload)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	seq, err := tx.publish(env)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	mutate := func(t Task) (Task, error) {
		t.Status = StatusClaimed
		t.ClaimedBy = agentID
		t.ClaimEpoch = claimEpoch
		t.UpdatedAt = time.Now().UTC()
		t.LastEventSeq = seq
		return t, nil
	}
	if err := tx.mgr.update(tx.ctx, tx.taskID, mutate); err != nil {
		tx.recordErr(err)
		return err
	}
	return nil
}

// Unclaim publishes an unclaimed event and updates the KV record's
// status to open and clears ClaimedBy. The free-form reason is
// recorded on the event but not stamped on the KV — operators recover
// it from the log.
func (tx *Tx) Unclaim(reason string) error {
	tx.markDirty()
	payload := UnclaimedPayload{Reason: reason}
	env, err := EncodeEnvelope(EventTypeUnclaimed, tx.taskID, payload)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	seq, err := tx.publish(env)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	mutate := func(t Task) (Task, error) {
		t.Status = StatusOpen
		t.ClaimedBy = ""
		t.UpdatedAt = time.Now().UTC()
		t.LastEventSeq = seq
		return t, nil
	}
	if err := tx.mgr.update(tx.ctx, tx.taskID, mutate); err != nil {
		tx.recordErr(err)
		return err
	}
	return nil
}

// Update publishes an updated event carrying one or more (field, old,
// new) tuples and applies the mutator to the KV record. The mutator
// is responsible for actually writing the new field values; the
// FieldChange tuples are descriptive (for the log) and must agree
// with the projection the mutator produces. Callers compose the
// mutate closure to keep the legacy invariant-checking semantics; the
// FieldChange tuples are caller-provided too.
func (tx *Tx) Update(
	mutate func(Task) (Task, error),
	changes ...FieldChange,
) error {
	tx.markDirty()
	if len(changes) == 0 {
		err := errors.New("tasks.Tx.Update: no field changes")
		tx.recordErr(err)
		return err
	}
	if mutate == nil {
		err := errors.New("tasks.Tx.Update: mutate is nil")
		tx.recordErr(err)
		return err
	}
	payload := UpdatedPayload{Changes: changes}
	env, err := EncodeEnvelope(EventTypeUpdated, tx.taskID, payload)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	seq, err := tx.publish(env)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	wrapped := func(t Task) (Task, error) {
		next, err := mutate(t)
		if err != nil {
			return t, err
		}
		next.UpdatedAt = time.Now().UTC()
		next.LastEventSeq = seq
		return next, nil
	}
	if err := tx.mgr.update(tx.ctx, tx.taskID, wrapped); err != nil {
		tx.recordErr(err)
		return err
	}
	return nil
}

// Link publishes a linked event and updates the KV record's Edges
// slice. Duplicate edges are caller-prevented (coord.Link enforces
// invariant 25); this method does not re-check.
func (tx *Tx) Link(otherID, edgeType string) error {
	tx.markDirty()
	if otherID == "" {
		err := errors.New("tasks.Tx.Link: otherID is empty")
		tx.recordErr(err)
		return err
	}
	if edgeType == "" {
		err := errors.New("tasks.Tx.Link: edgeType is empty")
		tx.recordErr(err)
		return err
	}
	payload := LinkedPayload{OtherID: otherID, EdgeType: edgeType}
	env, err := EncodeEnvelope(EventTypeLinked, tx.taskID, payload)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	seq, err := tx.publish(env)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	mutate := func(t Task) (Task, error) {
		t.Edges = append(t.Edges, Edge{
			Type:   EdgeType(edgeType),
			Target: otherID,
		})
		t.UpdatedAt = time.Now().UTC()
		t.LastEventSeq = seq
		return t, nil
	}
	if err := tx.mgr.update(tx.ctx, tx.taskID, mutate); err != nil {
		tx.recordErr(err)
		return err
	}
	return nil
}

// SlotChange publishes a slot_changed event. The KV projection does
// not store slot directly (slots are an external swarm concept); the
// event is purely audit. From may be empty for first slot bind.
func (tx *Tx) SlotChange(from, to string) error {
	tx.markDirty()
	if to == "" {
		err := errors.New("tasks.Tx.SlotChange: to is empty")
		tx.recordErr(err)
		return err
	}
	payload := SlotChangedPayload{From: from, To: to}
	env, err := EncodeEnvelope(EventTypeSlotChanged, tx.taskID, payload)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	if _, err := tx.publish(env); err != nil {
		tx.recordErr(err)
		return err
	}
	return nil
}

// Close publishes a closed event and updates the KV record into the
// terminal closed state. The mutator-shaped logic (invariant 12,
// invariant 24, agent identity check) lives on the caller side;
// tx.Close accepts a mutate callback so coord-level guards stay where
// they belong.
func (tx *Tx) Close(agentID, reason string, mutate func(Task) (Task, error)) error {
	tx.markDirty()
	if mutate == nil {
		err := errors.New("tasks.Tx.Close: mutate is nil")
		tx.recordErr(err)
		return err
	}
	payload := ClosedPayload{AgentID: agentID, Reason: reason}
	env, err := EncodeEnvelope(EventTypeClosed, tx.taskID, payload)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	seq, err := tx.publish(env)
	if err != nil {
		tx.recordErr(err)
		return err
	}
	wrapped := func(t Task) (Task, error) {
		next, err := mutate(t)
		if err != nil {
			return t, err
		}
		next.LastEventSeq = seq
		return next, nil
	}
	if err := tx.mgr.update(tx.ctx, tx.taskID, wrapped); err != nil {
		tx.recordErr(err)
		return err
	}
	return nil
}

// Mutate is a generic Tx-only escape hatch for mutators that do not
// fit the named tx.X verbs. It still publishes an updated event
// describing the changes so the log remains the source of truth. Used
// internally by the migration shim and by coord paths that bundle
// multiple field changes (e.g., Reclaim's claimed-by + claim-epoch
// pair).
//
// changes describes the (field, old, new) tuples for the event;
// mutate produces the new Task value. Both must agree.
func (tx *Tx) Mutate(
	mutate func(Task) (Task, error),
	changes ...FieldChange,
) error {
	return tx.Update(mutate, changes...)
}

// publish sends env on the events stream and returns the assigned
// stream sequence. When the manager has no event log configured (an
// archive-only Manager), publish returns 0 with ErrEventLogDisabled.
func (tx *Tx) publish(env EventEnvelope) (uint64, error) {
	if tx.mgr.stream == nil {
		return 0, ErrEventLogDisabled
	}
	body, err := MarshalEnvelope(env)
	if err != nil {
		return 0, fmt.Errorf("tasks.Tx.publish: marshal: %w", err)
	}
	msg := &nats.Msg{
		Subject: EventSubject(env.TaskID),
		Data:    body,
	}
	ack, err := tx.mgr.js.PublishMsg(tx.ctx, msg)
	if err != nil {
		return 0, fmt.Errorf("tasks.Tx.publish: %w", err)
	}
	return ack.Sequence, nil
}

// FieldChangeFromAny builds a FieldChange tuple from any-typed old/new
// values, marshaling each to JSON. Convenient for Update callers that
// already have the typed values in hand.
func FieldChangeFromAny(field string, oldV, newV any) (FieldChange, error) {
	oldRaw, err := json.Marshal(oldV)
	if err != nil {
		return FieldChange{}, fmt.Errorf("tasks: marshal old: %w", err)
	}
	newRaw, err := json.Marshal(newV)
	if err != nil {
		return FieldChange{}, fmt.Errorf("tasks: marshal new: %w", err)
	}
	return FieldChange{Field: field, Old: oldRaw, New: newRaw}, nil
}

// MustFieldChange is FieldChangeFromAny that panics on encode failure.
// Use only for values whose JSON shape is statically known (strings,
// integers, simple structs); a panic here is a programmer bug.
func MustFieldChange(field string, oldV, newV any) FieldChange {
	fc, err := FieldChangeFromAny(field, oldV, newV)
	if err != nil {
		panic(fmt.Sprintf("tasks.MustFieldChange: %v", err))
	}
	return fc
}
