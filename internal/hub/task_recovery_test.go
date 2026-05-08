package hub

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/testutil/natstest"
)

// jetstreamFor returns a JetStream handle bound to nc. Used by the
// recovery integration test to publish an orphan event directly
// (bypassing tasks.Tx, which would also write the KV).
func jetstreamFor(nc *nats.Conn) (jetstream.JetStream, error) {
	return jetstream.New(nc)
}

// newTestHubLogger returns a hubLogger whose file pointer is nil so
// every Infof/Warnf/Errorf call no-ops. Adequate for the recovery
// integration test — we assert behavior, not log output.
func newTestHubLogger(t *testing.T) *hubLogger {
	t.Helper()
	return &hubLogger{}
}

// TestRunTaskRecovery_HubStart_ReplaysOrphanEvent simulates the
// integration the reviewer asked for: a hub start with one orphan
// event in the stream + a matching projection at LastEventSeq <
// event seq. After runTaskRecovery returns, the projection is
// reconciled. This is the C1 acceptance test — it verifies the hub-
// start recovery wiring works end-to-end through Recover (gated by
// RecoverOnOpen) before any CLI verb could connect.
func TestRunTaskRecovery_HubStart_ReplaysOrphanEvent(t *testing.T) {
	srv, cleanup := natstest.NewJetStreamServer(t)
	defer cleanup()
	natsURL := srv.ConnectedUrl()

	ctx := context.Background()

	// Pre-state: open a tasks Manager (without RecoverOnOpen),
	// seed a task, simulate an orphan claimed event.
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	m, err := tasks.Open(ctx, nc, tasks.Config{
		BucketName:     tasks.DefaultBucketName,
		HistoryDepth:   8,
		MaxValueSize:   64 * 1024,
		EnableEventLog: true,
	})
	if err != nil {
		nc.Close()
		t.Fatalf("Open: %v", err)
	}
	taskID := "bones-hub-recover-1"
	rec := tasks.Task{
		ID:            taskID,
		Title:         "hub-recover example",
		Status:        tasks.StatusOpen,
		Files:         []string{"/work/x.go"},
		SchemaVersion: tasks.SchemaVersion,
	}
	if err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
		return tx.Create(rec)
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate orphan event (publish without KV write).
	env, err := tasks.EncodeEnvelope(tasks.EventTypeClaimed, taskID,
		tasks.ClaimedPayload{AgentID: "post-crash-agent", ClaimEpoch: 9})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	body, err := tasks.MarshalEnvelope(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js, err := jetstreamFor(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.Publish(ctx, tasks.EventSubject(taskID), body); err != nil {
		t.Fatalf("publish: %v", err)
	}
	_ = m.Close()
	nc.Close()

	// Pre-recovery: KV still shows the task as Open (the orphan
	// event has not been applied).
	got := readTaskForTest(t, natsURL, taskID)
	if got.Status != tasks.StatusOpen {
		t.Fatalf("pre-hub-start want Open, got %s", got.Status)
	}

	// Drive the hub-start recovery path. runTaskRecovery is the
	// helper hub.Start calls right after `hub: ready` and before
	// registry.Write makes the hub discoverable.
	hl := newTestHubLogger(t)
	runTaskRecovery(ctx, natsURL, hl)

	// Post-recovery: projection caught up with the orphan event.
	got = readTaskForTest(t, natsURL, taskID)
	if got.Status != tasks.StatusClaimed {
		t.Fatalf("post-hub-start want Claimed, got %s", got.Status)
	}
	if got.ClaimedBy != "post-crash-agent" {
		t.Fatalf("post-hub-start ClaimedBy = %q, want post-crash-agent",
			got.ClaimedBy)
	}
}

// readTaskForTest opens a fresh tasks.Manager (without recovery to
// avoid double-Recover noise) just to read the projection. Returns
// the decoded Task.
func readTaskForTest(t *testing.T, natsURL, id string) tasks.Task {
	t.Helper()
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("readTaskForTest: nats.Connect: %v", err)
	}
	defer nc.Close()
	m, err := tasks.Open(context.Background(), nc, tasks.Config{
		BucketName:     tasks.DefaultBucketName,
		HistoryDepth:   8,
		MaxValueSize:   64 * 1024,
		EnableEventLog: true,
	})
	if err != nil {
		t.Fatalf("readTaskForTest: Open: %v", err)
	}
	defer func() { _ = m.Close() }()
	got, _, err := m.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("readTaskForTest: Get: %v", err)
	}
	return got
}
