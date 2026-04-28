package swarm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// uniqueBucket returns a per-test bucket name so parallel runs do not
// share state. Mirrors internal/presence/presence_test.go.
func uniqueBucket(t *testing.T) string {
	t.Helper()
	return "bones-swarm-test-" + strings.ReplaceAll(t.Name(), "/", "-")
}

func openManager(t *testing.T, nc *nats.Conn) *Manager {
	t.Helper()
	m, err := Open(context.Background(), Config{
		NATSConn:   nc,
		BucketName: uniqueBucket(t),
		TTL:        time.Minute,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func sampleSession(slot, taskID string) Session {
	now := time.Now().UTC().Truncate(time.Second)
	return Session{
		Slot:        slot,
		TaskID:      taskID,
		AgentID:     "slot-" + slot,
		Host:        "test-host",
		LeafPID:     1234,
		StartedAt:   now,
		LastRenewed: now,
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("nil NATSConn must fail validation")
	}
}

func TestPutGet(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m := openManager(t, nc)

	sess := sampleSession("rendering", "task-rendering-1")
	if err := m.Put(context.Background(), sess); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, rev, err := m.Get(context.Background(), "rendering")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rev == 0 {
		t.Fatal("expected non-zero revision")
	}
	if got.Slot != sess.Slot || got.TaskID != sess.TaskID || got.AgentID != sess.AgentID {
		t.Fatalf("session mismatch: got=%+v want=%+v", got, sess)
	}
}

func TestGet_NotFound(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m := openManager(t, nc)

	_, _, err := m.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: want ErrNotFound, got %v", err)
	}
}

func TestPut_DuplicateConflict(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m := openManager(t, nc)

	sess := sampleSession("audio", "task-audio-1")
	if err := m.Put(context.Background(), sess); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := m.Put(context.Background(), sess); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("second Put: want ErrCASConflict, got %v", err)
	}
}

func TestUpdate_CAS(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m := openManager(t, nc)

	sess := sampleSession("physics", "task-physics-1")
	if err := m.Put(context.Background(), sess); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, rev, err := m.Get(context.Background(), "physics")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.LastRenewed = time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	if err := m.Update(context.Background(), got, rev); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Second Update with the now-stale revision must conflict.
	if err := m.Update(context.Background(), got, rev); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("stale Update: want ErrCASConflict, got %v", err)
	}
}

func TestDelete_CAS(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m := openManager(t, nc)

	sess := sampleSession("ui", "task-ui-1")
	if err := m.Put(context.Background(), sess); err != nil {
		t.Fatalf("Put: %v", err)
	}
	_, rev, err := m.Get(context.Background(), "ui")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := m.Delete(context.Background(), "ui", rev); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, _, err := m.Get(context.Background(), "ui"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post-delete Get: want ErrNotFound, got %v", err)
	}
}

func TestList(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m := openManager(t, nc)

	for _, slot := range []string{"a", "b", "c"} {
		if err := m.Put(context.Background(), sampleSession(slot, "task-"+slot)); err != nil {
			t.Fatalf("Put %s: %v", slot, err)
		}
	}
	got, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List: want 3 sessions, got %d (%+v)", len(got), got)
	}
}

func TestSlotDirHelpers(t *testing.T) {
	dir := SlotDir("/ws", "rendering")
	if !strings.HasSuffix(dir, "/.bones/swarm/rendering") {
		t.Errorf("SlotDir: %s", dir)
	}
	wt := SlotWorktree("/ws", "rendering")
	if !strings.HasSuffix(wt, "/.bones/swarm/rendering/wt") {
		t.Errorf("SlotWorktree: %s", wt)
	}
	pid := SlotPidFile("/ws", "rendering")
	if !strings.HasSuffix(pid, "/.bones/swarm/rendering/leaf.pid") {
		t.Errorf("SlotPidFile: %s", pid)
	}
}

func TestClose_ReturnsErrClosedAfter(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m := openManager(t, nc)

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, err := m.Get(context.Background(), "x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: want ErrClosed, got %v", err)
	}
}
