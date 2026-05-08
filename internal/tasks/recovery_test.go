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
