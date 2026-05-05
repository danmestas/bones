// Integration tests for `bones tasks close` covering the four
// auto-release cases from issue #209 (Layer C):
//
//  1. Slot-leaf claim, clean (commit since join) → task closed AND
//     swarm session record deleted in one step.
//  2. Slot-leaf claim, dirty (no commit since join) → tasks close
//     refuses, task state unchanged, error names the artifact gate.
//  3. --keep-slot on a slot-leaf claim → task closed, session record
//     intact, regardless of slot state.
//  4. Non-slot claim (claimed_by doesn't match "<slot>-leaf") →
//     today's behavior, slot session lookup never triggered.
//
// Fixture mirrors internal/swarm/lease_test.go (real NATS + real
// libfossil hub, no mocks; ADR 0030 discipline).
package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
	"github.com/danmestas/bones/internal/wspath"
)

// closeFixture brings up a workspace dir with an in-process libfossil
// hub plus a connected swarm.Sessions handle so tasks_close tests can
// drive both the tasks-bucket close and the swarm-session lookup
// against real substrate.
type closeFixture struct {
	dir  string
	hub  *coord.Hub
	info workspace.Info
}

// closeFreePort returns a random unused TCP port. Mirrors freePort in
// internal/swarm/lease_test.go (cli is a separate package so we
// duplicate the helper rather than expose it).
func closeFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func newCloseFixture(t *testing.T) *closeFixture {
	t.Helper()
	dir := t.TempDir()
	orch := filepath.Join(dir, ".bones")
	if err := os.MkdirAll(orch, 0o755); err != nil {
		t.Fatalf("mkdir .bones: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	hub, err := coord.OpenHub(ctx, orch, closeFreePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })
	return &closeFixture{
		dir: dir,
		hub: hub,
		info: workspace.Info{
			WorkspaceDir: dir,
			NATSURL:      hub.NATSURL(),
			AgentID:      "test-agent",
		},
	}
}

// createTask opens a fixture leaf and uses it to insert an open task
// in the tasks bucket so tests have something to claim/close.
func (f *closeFixture) createTask(t *testing.T, title, holdPath string) coord.TaskID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	leaf, err := coord.OpenLeaf(ctx, coord.LeafConfig{
		Hub:     f.hub,
		Workdir: filepath.Join(f.dir, ".bones", "fixture"),
		SlotID:  "fixture-" + title,
	})
	if err != nil {
		t.Fatalf("fixture OpenLeaf: %v", err)
	}
	defer func() { _ = leaf.Stop() }()
	tid, err := leaf.OpenTask(ctx, title, []string{holdPath})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	return tid
}

// acquireAndCommit runs Acquire → Release → Resume → Commit → Release
// on the fixture's hub so the slot has a clean session record AND a
// landed commit (artifact precondition satisfied for a success close).
func (f *closeFixture) acquireAndCommit(t *testing.T, slot, taskID, holdPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := swarm.Acquire(ctx, f.info, slot, taskID, swarm.AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	resumed, err := swarm.Resume(ctx, f.info, slot, swarm.AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	files := []coord.File{{
		Path:    wspath.Must(holdPath),
		Name:    filepath.Base(holdPath),
		Content: []byte("artifact for tasks-close-209"),
	}}
	if _, err := resumed.Commit(ctx, "task close test artifact", files); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := resumed.Release(ctx); err != nil {
		t.Fatalf("post-commit Release: %v", err)
	}
}

// acquireOnly runs Acquire → Release without a commit so the
// artifact precondition will refuse a success close (LastRenewed ==
// StartedAt). Mirrors the "agent crashed before its first commit" shape.
func (f *closeFixture) acquireOnly(t *testing.T, slot, taskID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := swarm.Acquire(ctx, f.info, slot, taskID, swarm.AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// readTask reads the task record. Used to verify task state pre/post
// runClose.
func (f *closeFixture) readTask(t *testing.T, taskID string) tasks.Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nc, err := nats.Connect(f.info.NATSURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	mgr, err := tasks.Open(ctx, nc, tasks.Config{
		BucketName:   tasks.DefaultBucketName,
		HistoryDepth: 10,
		MaxValueSize: 64 * 1024,
		ChanBuffer:   32,
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	task, _, err := mgr.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("tasks.Get: %v", err)
	}
	return task
}

// sessionExists returns true if the swarm session record for slot is
// still in KV. Used to verify auto-release deleted the record.
func (f *closeFixture) sessionExists(t *testing.T, slot string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nc, err := nats.Connect(f.info.NATSURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	sess, err := swarm.Open(ctx, swarm.Config{NATSConn: nc})
	if err != nil {
		t.Fatalf("swarm.Open: %v", err)
	}
	defer func() { _ = sess.Close() }()
	_, _, err = sess.Get(ctx, slot)
	if err == nil {
		return true
	}
	if errors.Is(err, swarm.ErrNotFound) {
		return false
	}
	t.Fatalf("sessions.Get: %v", err)
	return false
}

// TestTasksClose_SlotLeafClean covers acceptance criterion 1: when the
// task's claimed_by ends in `<slot>-leaf` and the slot has committed
// at least once since join, `tasks close` runs the swarm-close path —
// the task is closed AND the session record is deleted.
func TestTasksClose_SlotLeafClean(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	f := newCloseFixture(t)
	holdPath := filepath.Join(f.dir, "alpha", "artifact.txt")
	taskID := string(f.createTask(t, "alpha-task", holdPath))
	f.acquireAndCommit(t, "alpha", taskID, holdPath)

	if !f.sessionExists(t, "alpha") {
		t.Fatal("precondition: session record should exist before close")
	}

	cmd := &TasksCloseCmd{ID: taskID, Reason: "done"}
	if err := cmd.runClose(context.Background(), f.info); err != nil {
		t.Fatalf("runClose: %v", err)
	}

	got := f.readTask(t, taskID)
	if got.Status != tasks.StatusClosed {
		t.Errorf("task status: got %q want %q", got.Status, tasks.StatusClosed)
	}
	if got.ClaimedBy != "" {
		t.Errorf("ClaimedBy after close: got %q want empty (invariant 11)", got.ClaimedBy)
	}
	if f.sessionExists(t, "alpha") {
		t.Error("session record should be deleted after auto-release")
	}
}

// TestTasksClose_SlotLeafDirtyRefuses covers acceptance criterion 2:
// when the slot-leaf claim is matched but the artifact precondition
// would refuse the swarm close, tasks close itself refuses and the
// task record is NOT updated. Atomicity across both layers.
func TestTasksClose_SlotLeafDirtyRefuses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	f := newCloseFixture(t)
	holdPath := filepath.Join(f.dir, "beta", "missing.txt")
	taskID := string(f.createTask(t, "beta-task", holdPath))
	f.acquireOnly(t, "beta", taskID)

	preTask := f.readTask(t, taskID)
	if preTask.Status == tasks.StatusClosed {
		t.Fatalf("precondition: task already closed")
	}
	preClaim := preTask.ClaimedBy
	if !f.sessionExists(t, "beta") {
		t.Fatal("precondition: session record should exist before close")
	}

	cmd := &TasksCloseCmd{ID: taskID, Reason: "premature"}
	err := cmd.runClose(context.Background(), f.info)
	if err == nil {
		t.Fatal("runClose: want error (artifact precondition refuses), got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "swarm") && !strings.Contains(msg, "artifact") &&
		!strings.Contains(msg, "commit since join") {
		t.Errorf("error should name the artifact gate; got %q", msg)
	}
	if !strings.Contains(msg, "beta") {
		t.Errorf("error should name the slot beta; got %q", msg)
	}

	// Task state must be unchanged across the refused close.
	postTask := f.readTask(t, taskID)
	if postTask.Status == tasks.StatusClosed {
		t.Errorf("task should not be closed on refused tasks close: status=%q", postTask.Status)
	}
	if postTask.ClaimedBy != preClaim {
		t.Errorf("ClaimedBy mutated: got %q want %q", postTask.ClaimedBy, preClaim)
	}
	if !f.sessionExists(t, "beta") {
		t.Error("session record should still exist on refused close")
	}
}

// TestTasksClose_KeepSlot covers acceptance criterion 3: --keep-slot
// closes the task and leaves the swarm session intact regardless of
// slot state. Even on a "would-refuse" slot (no commit since join),
// --keep-slot succeeds because it skips the slot-side gate entirely.
func TestTasksClose_KeepSlot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	f := newCloseFixture(t)
	holdPath := filepath.Join(f.dir, "gamma", "scratch.txt")
	taskID := string(f.createTask(t, "gamma-task", holdPath))
	f.acquireOnly(t, "gamma", taskID)

	cmd := &TasksCloseCmd{ID: taskID, Reason: "operator-keep", KeepSlot: true}
	if err := cmd.runClose(context.Background(), f.info); err != nil {
		t.Fatalf("runClose --keep-slot: %v", err)
	}

	got := f.readTask(t, taskID)
	if got.Status != tasks.StatusClosed {
		t.Errorf("task status: got %q want %q", got.Status, tasks.StatusClosed)
	}
	if got.ClosedReason != "operator-keep" {
		t.Errorf("ClosedReason: got %q want %q", got.ClosedReason, "operator-keep")
	}
	if !f.sessionExists(t, "gamma") {
		t.Error("session record should still exist with --keep-slot")
	}
}

// TestTasksClose_NonSlotClaim covers acceptance criterion 4: a task
// whose claimed_by is NOT a slot-leaf identity (e.g. a manual claim
// via `bones tasks claim`) closes via the existing path with no slot
// lookup, no session interaction. No regression on today's behavior.
func TestTasksClose_NonSlotClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS test in -short mode")
	}
	f := newCloseFixture(t)
	holdPath := filepath.Join(f.dir, "manual", "thing.txt")
	taskID := string(f.createTask(t, "manual-task", holdPath))

	// Seed a non-slot claim by writing the task record directly via
	// the tasks Manager. The "claimed_by" string ("operator-x")
	// deliberately has no -leaf suffix so the auto-release branch
	// must not fire.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nc, err := nats.Connect(f.info.NATSURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	mgr, err := tasks.Open(ctx, nc, tasks.Config{
		BucketName:   tasks.DefaultBucketName,
		HistoryDepth: 10,
		MaxValueSize: 64 * 1024,
		ChanBuffer:   32,
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	if err := mgr.Update(ctx, taskID, func(cur tasks.Task) (tasks.Task, error) {
		cur.Status = tasks.StatusClaimed
		cur.ClaimedBy = "operator-x"
		cur.UpdatedAt = time.Now().UTC()
		return cur, nil
	}); err != nil {
		t.Fatalf("seed manual claim: %v", err)
	}

	cmd := &TasksCloseCmd{ID: taskID, Reason: "manual"}
	if err := cmd.runClose(context.Background(), f.info); err != nil {
		t.Fatalf("runClose: %v", err)
	}

	got := f.readTask(t, taskID)
	if got.Status != tasks.StatusClosed {
		t.Errorf("task status: got %q want %q", got.Status, tasks.StatusClosed)
	}
	if got.ClosedReason != "manual" {
		t.Errorf("ClosedReason: got %q want %q (legacy path preserves --reason)",
			got.ClosedReason, "manual")
	}
	if got.ClosedBy != "operator-x" {
		t.Errorf("ClosedBy: got %q want %q (legacy path attributes to claimer)",
			got.ClosedBy, "operator-x")
	}
}
