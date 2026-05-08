package tasks_test

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/testutil/natstest"
)

// getJetStream is a tiny helper that returns a JetStream handle on nc.
// The recovery test publishes an orphan event directly (without going
// through Tx) so it needs its own handle.
func getJetStream(nc *nats.Conn) (jetstream.JetStream, error) {
	return jetstream.New(nc)
}

// TestRecovery_OrphanedEventReplays simulates a hub crash between
// publish and KV-write by emitting a synthetic event directly to the
// stream and asserting Recover updates the KV projection. The shape
// mirrors what Tx would produce on a successful publish + failed CAS
// write.
func TestRecovery_OrphanedEventReplays(t *testing.T) {
	srv, cleanup := natstest.NewJetStreamServer(t)
	defer cleanup()
	nc, err := nats.Connect(srv.ConnectedUrl())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	cfg := tasks.Config{
		BucketName:     "tasks-recover-test",
		HistoryDepth:   8,
		MaxValueSize:   8 * 1024,
		EnableEventLog: true,
	}
	m, err := tasks.Open(context.Background(), nc, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx := context.Background()

	// Seed a task via Tx.
	rec := txNewTask("bones-recover-1")
	if err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
		return tx.Create(rec)
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Simulate an orphan event: publish a `claimed` envelope directly
	// without going through Tx (which would also write the KV). The
	// projection now lags the log.
	env, err := tasks.EncodeEnvelope(
		tasks.EventTypeClaimed, rec.ID,
		tasks.ClaimedPayload{AgentID: "ghost-agent", ClaimEpoch: 7},
	)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	body, err := tasks.MarshalEnvelope(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := nc
	_ = js
	// Use a fresh JetStream handle — we want the publish path the
	// recovery test isolates, not Tx.
	jsHandle, err := getJetStream(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := jsHandle.Publish(ctx, tasks.EventSubject(rec.ID), body); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Pre-recovery: KV still shows the task as Open.
	got, _, err := m.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get pre-recover: %v", err)
	}
	if got.Status != tasks.StatusOpen {
		t.Fatalf("pre-recover want Open, got %s", got.Status)
	}

	// Run recovery.
	replayed, err := tasks.Recover(ctx, m)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if replayed == 0 {
		t.Fatalf("expected at least one event replayed")
	}

	// Post-recovery: KV reflects the orphan event's effect.
	got, _, err = m.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get post-recover: %v", err)
	}
	if got.Status != tasks.StatusClaimed {
		t.Fatalf("post-recover want Claimed, got %s", got.Status)
	}
	if got.ClaimedBy != "ghost-agent" {
		t.Fatalf("post-recover ClaimedBy = %q, want ghost-agent", got.ClaimedBy)
	}
}

// publishOrphanUpdated publishes an updated event directly to the
// stream, simulating a hub crash between Tx publish and KV write.
// Returns the seeded Manager and task so per-field assertions can
// run against the projection post-Recover.
func publishOrphanUpdated(
	t *testing.T,
	taskID string,
	changes []tasks.FieldChange,
) (*tasks.Manager, func()) {
	t.Helper()
	srv, cleanup := natstest.NewJetStreamServer(t)
	nc, err := nats.Connect(srv.ConnectedUrl())
	if err != nil {
		cleanup()
		t.Fatalf("nats.Connect: %v", err)
	}
	m, err := tasks.Open(context.Background(), nc, tasks.Config{
		BucketName:     "tasks-replay-field",
		HistoryDepth:   8,
		MaxValueSize:   8 * 1024,
		EnableEventLog: true,
	})
	if err != nil {
		nc.Close()
		cleanup()
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	rec := txNewTask(taskID)
	if err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
		return tx.Create(rec)
	}); err != nil {
		_ = m.Close()
		nc.Close()
		cleanup()
		t.Fatalf("create: %v", err)
	}
	env, err := tasks.EncodeEnvelope(tasks.EventTypeUpdated, rec.ID,
		tasks.UpdatedPayload{Changes: changes})
	if err != nil {
		_ = m.Close()
		nc.Close()
		cleanup()
		t.Fatalf("encode: %v", err)
	}
	body, err := tasks.MarshalEnvelope(env)
	if err != nil {
		_ = m.Close()
		nc.Close()
		cleanup()
		t.Fatalf("marshal: %v", err)
	}
	jsHandle, err := getJetStream(nc)
	if err != nil {
		_ = m.Close()
		nc.Close()
		cleanup()
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := jsHandle.Publish(ctx, tasks.EventSubject(rec.ID), body); err != nil {
		_ = m.Close()
		nc.Close()
		cleanup()
		t.Fatalf("publish: %v", err)
	}
	return m, func() {
		_ = m.Close()
		nc.Close()
		cleanup()
	}
}

// TestRecover_AppliesUpdatedEventField_Title verifies that an orphan
// updated event carrying a title FieldChange is applied to the
// projection on Recover. The log alone now reconstructs the
// post-mutation state — the property ADR 0052 §"Recovery" promises.
func TestRecover_AppliesUpdatedEventField_Title(t *testing.T) {
	taskID := "bones-replay-title"
	changes := []tasks.FieldChange{
		tasks.MustFieldChange("title", "tx example", "renamed via orphan"),
	}
	m, cleanup := publishOrphanUpdated(t, taskID, changes)
	defer cleanup()
	ctx := context.Background()
	if _, err := tasks.Recover(ctx, m); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got, _, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "renamed via orphan" {
		t.Fatalf("title not applied: got %q", got.Title)
	}
}

// TestRecover_AppliesUpdatedEventField_ClaimedBy verifies a coupled
// (status, claimed_by) FieldChange pair flows through replay
// correctly. Invariant 11 (claimed_by non-empty iff status==claimed)
// is enforced by the KV write path — a real claim event always
// carries both fields, so replay does too.
func TestRecover_AppliesUpdatedEventField_ClaimedBy(t *testing.T) {
	taskID := "bones-replay-claimedby"
	changes := []tasks.FieldChange{
		tasks.MustFieldChange("status", tasks.StatusOpen, tasks.StatusClaimed),
		tasks.MustFieldChange("claimed_by", "", "alice"),
	}
	m, cleanup := publishOrphanUpdated(t, taskID, changes)
	defer cleanup()
	ctx := context.Background()
	if _, err := tasks.Recover(ctx, m); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got, _, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ClaimedBy != "alice" {
		t.Fatalf("claimed_by not applied: got %q", got.ClaimedBy)
	}
	if got.Status != tasks.StatusClaimed {
		t.Fatalf("status not applied: got %s", got.Status)
	}
}

// TestRecover_AppliesUpdatedEventField_Files exercises a slice-typed
// field to verify the JSON unmarshal handles non-scalars correctly.
func TestRecover_AppliesUpdatedEventField_Files(t *testing.T) {
	taskID := "bones-replay-files"
	newFiles := []string{"/work/a.go", "/work/b.go", "/work/c.go"}
	changes := []tasks.FieldChange{
		tasks.MustFieldChange("files", []string{"/work/x.go"}, newFiles),
	}
	m, cleanup := publishOrphanUpdated(t, taskID, changes)
	defer cleanup()
	ctx := context.Background()
	if _, err := tasks.Recover(ctx, m); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got, _, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Files) != len(newFiles) {
		t.Fatalf("files len: want %d got %d", len(newFiles), len(got.Files))
	}
	for i, f := range newFiles {
		if got.Files[i] != f {
			t.Fatalf("files[%d]: want %q got %q", i, f, got.Files[i])
		}
	}
}

// TestRecover_AppliesUpdatedEventField_Context exercises a map-typed
// field to verify the JSON unmarshal handles map values.
func TestRecover_AppliesUpdatedEventField_Context(t *testing.T) {
	taskID := "bones-replay-context"
	newCtx := map[string]string{"priority": "P0", "owner": "alice"}
	changes := []tasks.FieldChange{
		tasks.MustFieldChange("context", map[string]string{}, newCtx),
	}
	m, cleanup := publishOrphanUpdated(t, taskID, changes)
	defer cleanup()
	ctx := context.Background()
	if _, err := tasks.Recover(ctx, m); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got, _, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Context["priority"] != "P0" || got.Context["owner"] != "alice" {
		t.Fatalf("context not applied: got %+v", got.Context)
	}
}

// TestRecover_AppliesUpdatedEventField_MultipleChanges verifies a
// single updated event with multiple FieldChange tuples applies all
// of them in one CAS write. The shape mimics a CLI bones-tasks-update
// invocation that changes several fields at once.
func TestRecover_AppliesUpdatedEventField_MultipleChanges(t *testing.T) {
	taskID := "bones-replay-multi"
	changes := []tasks.FieldChange{
		tasks.MustFieldChange("title", "tx example", "multi-renamed"),
		tasks.MustFieldChange("parent", "", "bones-parent-1"),
	}
	m, cleanup := publishOrphanUpdated(t, taskID, changes)
	defer cleanup()
	ctx := context.Background()
	if _, err := tasks.Recover(ctx, m); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got, _, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "multi-renamed" {
		t.Fatalf("title: got %q", got.Title)
	}
	if got.Parent != "bones-parent-1" {
		t.Fatalf("parent: got %q", got.Parent)
	}
}

// TestRecover_AppliesUpdatedEventField_UnknownFieldDropped verifies
// that an unknown field name in a FieldChange is silently dropped
// per the additive-only evolution rule.
func TestRecover_AppliesUpdatedEventField_UnknownFieldDropped(t *testing.T) {
	taskID := "bones-replay-unknown"
	changes := []tasks.FieldChange{
		tasks.MustFieldChange("future_field", nil, "new-value"),
	}
	m, cleanup := publishOrphanUpdated(t, taskID, changes)
	defer cleanup()
	ctx := context.Background()
	if _, err := tasks.Recover(ctx, m); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// No assertion on Task content — the test passes if Recover did
	// not error on the unknown field. The projection's LastEventSeq
	// should advance even though no field was applied.
	got, _, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastEventSeq == 0 {
		t.Fatalf("LastEventSeq should advance even for unknown-field events")
	}
}
