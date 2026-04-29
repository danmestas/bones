package dispatch

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/testutil/natstest"
	"github.com/nats-io/nats.go"
)

func newCoordOnURLWithHeartbeat(
	t *testing.T, url, agentID string, hb time.Duration,
) *coord.Coord {
	t.Helper()
	fileID := strings.ReplaceAll(agentID, "/", "_")
	cfg := coord.Config{
		AgentID:            agentID,
		NATSURL:            url,
		ChatFossilRepoPath: filepath.Join(t.TempDir(), fileID+"-chat.fossil"),
		CheckoutRoot:       filepath.Join(t.TempDir(), fileID+"-checkouts"),
		Tuning: coord.TuningConfig{
			HeartbeatInterval: hb,
		},
	}
	c, err := coord.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open(%s): %v", agentID, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func killAgentHeartbeat(t *testing.T, c *coord.Coord) {
	t.Helper()
	cv := reflect.ValueOf(c).Elem()
	subField := cv.FieldByName("sub")
	subPtr := reflect.NewAt(subField.Type(), unsafe.Pointer(subField.UnsafeAddr())).Elem()
	ncField := subPtr.Elem().FieldByName("nc")
	nc := reflect.NewAt(
		ncField.Type(), unsafe.Pointer(ncField.UnsafeAddr()),
	).Elem().Interface().(*nats.Conn)
	nc.Close()
}

func TestWaitWorkerAbsent_ReturnsWhenWorkerDropsFromPresence(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	parent := newCoordOnURLWithHeartbeat(
		t, nc.ConnectedUrl(), "parent-agent", 200*time.Millisecond,
	)
	worker := newCoordOnURLWithHeartbeat(
		t, nc.ConnectedUrl(), "parent-agent/task-1", 200*time.Millisecond,
	)
	ctx := context.Background()

	killAgentHeartbeat(t, worker)
	if err := WaitWorkerAbsent(
		ctx, parent.PresentAgentIDs, "parent-agent/task-1", 3*time.Second,
	); err != nil {
		t.Fatalf("WaitWorkerAbsent: %v", err)
	}
}

func TestReclaimClaimedTaskAfterWorkerDeath(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	parent := newCoordOnURLWithHeartbeat(
		t, nc.ConnectedUrl(), "parent-agent", 200*time.Millisecond,
	)
	worker := newCoordOnURLWithHeartbeat(
		t, nc.ConnectedUrl(), "parent-agent/task-1", 200*time.Millisecond,
	)
	ctx := context.Background()

	id, err := parent.OpenTask(ctx, "dispatch me", []string{"/repo/a.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := worker.Claim(ctx, id, time.Minute)
	if err != nil {
		t.Fatalf("worker Claim: %v", err)
	}
	_ = rel
	killAgentHeartbeat(t, worker)
	if err := WaitWorkerAbsent(
		ctx, parent.PresentAgentIDs, "parent-agent/task-1", 3*time.Second,
	); err != nil {
		t.Fatalf("WaitWorkerAbsent: %v", err)
	}
	relParent, err := parent.Reclaim(ctx, id, time.Minute)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	defer func() { _ = relParent() }()
}
