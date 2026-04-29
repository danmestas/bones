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

func openSessions(t *testing.T, nc *nats.Conn) *Sessions {
	t.Helper()
	s, err := Open(context.Background(), Config{
		NATSConn:   nc,
		BucketName: uniqueBucket(t),
		TTL:        time.Minute,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
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
	s := openSessions(t, nc)

	sess := sampleSession("rendering", "task-rendering-1")
	if err := s.put(context.Background(), sess); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, rev, err := s.Get(context.Background(), "rendering")
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
	s := openSessions(t, nc)

	_, _, err := s.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get: want ErrNotFound, got %v", err)
	}
}

func TestPut_DuplicateConflict(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	s := openSessions(t, nc)

	sess := sampleSession("audio", "task-audio-1")
	if err := s.put(context.Background(), sess); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := s.put(context.Background(), sess); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("second put: want ErrCASConflict, got %v", err)
	}
}

func TestUpdate_CAS(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	s := openSessions(t, nc)

	sess := sampleSession("physics", "task-physics-1")
	if err := s.put(context.Background(), sess); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, rev, err := s.Get(context.Background(), "physics")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.LastRenewed = time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	if err := s.update(context.Background(), got, rev); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Second update with the now-stale revision must conflict.
	if err := s.update(context.Background(), got, rev); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("stale update: want ErrCASConflict, got %v", err)
	}
}

func TestDelete_CAS(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	s := openSessions(t, nc)

	sess := sampleSession("ui", "task-ui-1")
	if err := s.put(context.Background(), sess); err != nil {
		t.Fatalf("put: %v", err)
	}
	_, rev, err := s.Get(context.Background(), "ui")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := s.delete(context.Background(), "ui", rev); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, _, err := s.Get(context.Background(), "ui"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post-delete Get: want ErrNotFound, got %v", err)
	}
}

func TestList(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	s := openSessions(t, nc)

	for _, slot := range []string{"a", "b", "c"} {
		if err := s.put(context.Background(), sampleSession(slot, "task-"+slot)); err != nil {
			t.Fatalf("put %s: %v", slot, err)
		}
	}
	got, err := s.List(context.Background())
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
	s := openSessions(t, nc)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, err := s.Get(context.Background(), "x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: want ErrClosed, got %v", err)
	}
}
