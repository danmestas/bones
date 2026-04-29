package swarm

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/workspace"
	"github.com/danmestas/bones/internal/wspath"
)

// freePort returns a random unused TCP port for an in-process hub.
// Mirrors internal/coord/hub_test.go's helper.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// leaseFixture brings up a workspace dir + an in-process hub
// (writing hub.fossil under .orchestrator) and returns the
// workspace.Info shape Acquire / Resume consume. Per ADR
// 0030 this uses real NATS + real Fossil — no mocks.
type leaseFixture struct {
	dir  string
	hub  *coord.Hub
	info workspace.Info
}

func newLeaseFixture(t *testing.T) *leaseFixture {
	t.Helper()
	dir := t.TempDir()
	orch := filepath.Join(dir, ".orchestrator")
	if err := os.MkdirAll(orch, 0o755); err != nil {
		t.Fatalf("mkdir orchestrator: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	hub, err := coord.OpenHub(ctx, orch, freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })
	return &leaseFixture{
		dir: dir,
		hub: hub,
		info: workspace.Info{
			WorkspaceDir: dir,
			NATSURL:      hub.NATSURL(),
		},
	}
}

// createTask inserts an open task on the hub so Acquire has
// something to claim. Uses a temporary fixture leaf to call
// coord.Leaf.OpenTask — same path the bones tasks-create CLI verb
// takes — so the substrate sees a fully-formed task record. The
// fixture leaf is closed before returning; the lease under test
// opens its own leaf inside Acquire.
func (f *leaseFixture) createTask(t *testing.T, title, holdPath string) coord.TaskID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixtureLeaf, err := coord.OpenLeaf(ctx, coord.LeafConfig{
		Hub:     f.hub,
		Workdir: filepath.Join(f.dir, ".bones", "fixture"),
		SlotID:  "fixture-" + title,
	})
	if err != nil {
		t.Fatalf("fixture OpenLeaf: %v", err)
	}
	defer func() { _ = fixtureLeaf.Stop() }()
	taskID, err := fixtureLeaf.OpenTask(ctx, title, []string{holdPath})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	return taskID
}

// TestAcquire_RefusesWithoutHubFossil pins the role-leak
// guard from PR #54. A workspace dir with no
// `.orchestrator/hub.fossil` MUST cause Acquire to return
// ErrWorkspaceNotBootstrapped without attempting any other work.
// The error string MUST NOT contain "run `bones up`" — that
// guidance is for orchestrators, not leaves.
func TestAcquire_RefusesWithoutHubFossil(t *testing.T) {
	dir := t.TempDir() // no .orchestrator/hub.fossil
	info := workspace.Info{WorkspaceDir: dir, NATSURL: "nats://127.0.0.1:1"}

	_, err := Acquire(context.Background(), info, "demo", "task-x", AcquireOpts{})
	if !errors.Is(err, ErrWorkspaceNotBootstrapped) {
		t.Fatalf("Acquire: want ErrWorkspaceNotBootstrapped, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "refusing to bootstrap from a leaf context") {
		t.Errorf("error missing leaf-context refusal: %s", msg)
	}
	if strings.Contains(msg, "run `bones up`") {
		t.Errorf("error contains orchestrator-targeted guidance: %s", msg)
	}
}

// TestAcquire_SuccessAndRelease covers the happy path: fresh
// acquire writes the session record + opens the leaf + claims the
// task; Release tears down the leaf without deleting the record.
func TestAcquire_SuccessAndRelease(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "rendering", "hello.txt")
	taskID := string(f.createTask(t, "rendering-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease, err := Acquire(ctx, f.info, "rendering", taskID, AcquireOpts{
		Hub: f.hub,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.Slot() != "rendering" {
		t.Errorf("Slot: got %q want %q", lease.Slot(), "rendering")
	}
	if lease.TaskID() != taskID {
		t.Errorf("TaskID: got %q want %q", lease.TaskID(), taskID)
	}
	if lease.WT() == "" {
		t.Errorf("WT: empty")
	}
	if lease.SessionRevision() == 0 {
		t.Errorf("SessionRevision: zero")
	}

	// Session record must be visible on a Sessions handle separate
	// from the one FreshLease owns internally.
	verifySess := openVerifySessions(t, f)
	got, _, err := verifySess.Get(ctx, "rendering")
	if err != nil {
		t.Fatalf("post-acquire Get: %v", err)
	}
	if got.TaskID != taskID {
		t.Errorf("session task: got %q want %q", got.TaskID, taskID)
	}
	if got.HubURL == "" {
		t.Errorf("session hub URL: empty")
	}

	// Pid file must exist.
	if _, err := os.Stat(SlotPidFile(f.dir, "rendering")); err != nil {
		t.Errorf("pid file missing: %v", err)
	}

	// Release should NOT delete the session record. Verify by opening
	// a fresh Sessions handle (different from the lease's, which
	// Release just closed) and reading the slot's record back.
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, _, err := verifySess.Get(ctx, "rendering"); err != nil {
		t.Errorf("session record gone after Release (expected to persist): %v", err)
	}

	// Release must be idempotent.
	if err := lease.Release(ctx); err != nil {
		t.Errorf("second Release: %v", err)
	}
}

// openVerifySessions dials NATS at the fixture hub's URL and opens a
// swarm.Sessions handle that the test owns the lifetime of. Used to
// read session records the lease wrote, after the lease's own
// Sessions handle has been closed by Release/Close.
func openVerifySessions(t *testing.T, f *leaseFixture) *Sessions {
	t.Helper()
	sess, _, _, err := openLeaseSessions(context.Background(), f.info, nil)
	if err != nil {
		t.Fatalf("openVerifySessions: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// TestAcquire_RefusesActiveLiveSession pins the
// ErrSessionAlreadyLive path: a live session on the same host
// without --force must be rejected.
func TestAcquire_RefusesActiveLiveSession(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "physics", "hello.txt")
	taskID := string(f.createTask(t, "physics-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "physics", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	t.Cleanup(func() { _ = first.Release(ctx) })

	// Without --force, second acquire must refuse.
	_, err = Acquire(ctx, f.info, "physics", taskID, AcquireOpts{Hub: f.hub})
	if !errors.Is(err, ErrSessionAlreadyLive) {
		t.Fatalf("second Acquire: want ErrSessionAlreadyLive, got %v", err)
	}
}

// TestResume_FailsWithoutSession ensures Resume on a slot that
// has no session record returns ErrSessionNotFound, not a generic
// substrate error.
func TestResume_FailsWithoutSession(t *testing.T) {
	f := newLeaseFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := Resume(ctx, f.info, "ghost", AcquireOpts{Hub: f.hub})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Resume: want ErrSessionNotFound, got %v", err)
	}
}

// TestResume_AfterAcquire confirms Resume reconstructs a
// usable lease from the session record Acquire wrote.
func TestResume_AfterAcquire(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "ui", "hello.txt")
	taskID := string(f.createTask(t, "ui-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "ui", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	resumed, err := Resume(ctx, f.info, "ui", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Cleanup(func() { _ = resumed.Release(ctx) })

	if resumed.Slot() != "ui" || resumed.TaskID() != taskID {
		t.Errorf("resumed lease: slot=%q task=%q want slot=%q task=%q",
			resumed.Slot(), resumed.TaskID(), "ui", taskID)
	}
	if resumed.HubURL() == "" {
		t.Errorf("resumed lease: hubURL empty")
	}
	if resumed.FossilUser() != "slot-ui" {
		t.Errorf("resumed lease: fossilUser=%q", resumed.FossilUser())
	}
}

// TestClose_DeletesRecordAndPidFile pins the Close contract:
// removes the session record (CAS-gated), removes the host-local
// pid file, and stops the leaf. After Close, Acquire on the
// same slot must succeed without --force.
//
// Under the FreshLease/ResumedLease split, Close is a ResumedLease
// method, so the flow is Acquire → Release (record persists) →
// Resume → Close, mirroring the new CLI invocation shape (acquire
// in one call, close in a separate call).
func TestClose_DeletesRecordAndPidFile(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "audio", "hello.txt")
	taskID := string(f.createTask(t, "audio-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "audio", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	pidPath := SlotPidFile(f.dir, "audio")
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("pre-Close pid file missing: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release after Acquire: %v", err)
	}

	resumed, err := Resume(ctx, f.info, "audio", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := resumed.Close(ctx, CloseOpts{CloseTaskOnSuccess: true}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("pid file should be gone after Close: err=%v", err)
	}

	// Second acquire on same slot without --force should now succeed.
	holdPath2 := filepath.Join(f.dir, "audio", "v2.txt")
	taskID2 := string(f.createTask(t, "audio-task-2", holdPath2))
	second, err := Acquire(ctx, f.info, "audio", taskID2, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("post-Close Acquire: %v", err)
	}
	t.Cleanup(func() { _ = second.Release(ctx) })
}

// TestCommit_SuccessUpdatesTrunkAndRenewsSession covers the happy
// path: claim → AnnounceHolds → Leaf.Commit → release → leaf stop
// → push to hub → renew session. UUID is non-empty, push result
// is set (in-process hub accepts the HTTP /xfer push), the
// session record's LastRenewed bumps.
func TestCommit_SuccessUpdatesTrunkAndRenewsSession(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "render", "hello.txt")
	taskID := string(f.createTask(t, "render-commit-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease, err := Acquire(ctx, f.info, "render", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release after Acquire: %v", err)
	}

	// Capture the pre-commit LastRenewed so we can compare.
	verifySess := openVerifySessions(t, f)
	preSess, _, err := verifySess.Get(ctx, "render")
	if err != nil {
		t.Fatalf("pre-commit Get: %v", err)
	}
	preRenewed := preSess.LastRenewed

	// Sleep a single tick so LastRenewed is observably newer.
	time.Sleep(10 * time.Millisecond)

	resumed, err := Resume(ctx, f.info, "render", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	wt := resumed.WT()
	if err := os.WriteFile(filepath.Join(wt, "hello.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	files := []coord.File{{
		Path:    wspath.Must(holdPath),
		Name:    "hello.txt",
		Content: []byte("world"),
	}}
	res, err := resumed.Commit(ctx, "test commit", files)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if res.UUID == "" {
		t.Errorf("Commit: empty UUID")
	}
	if res.PushErr != nil {
		t.Errorf("Commit: PushErr=%v (in-process hub should accept push)", res.PushErr)
	}
	if res.PushResult == nil {
		t.Errorf("Commit: PushResult nil")
	}
	if res.RenewErr != nil {
		t.Errorf("Commit: RenewErr=%v", res.RenewErr)
	}

	// Verify session record's LastRenewed bumped.
	postSess, _, err := verifySess.Get(ctx, "render")
	if err != nil {
		t.Fatalf("post-commit Get: %v", err)
	}
	if !postSess.LastRenewed.After(preRenewed) {
		t.Errorf("LastRenewed did not advance: pre=%v post=%v", preRenewed, postSess.LastRenewed)
	}

	if err := resumed.Release(ctx); err != nil {
		t.Errorf("Release: %v", err)
	}
}

// TestResume_RefusesCrossHost pins the new Resume cross-host
// guard. A session whose Host field doesn't match this machine
// must be rejected with ErrCrossHostOperation rather than
// reconstructing a leaf the caller can't actually drive.
func TestResume_RefusesCrossHost(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "ghosthost", "hello.txt")
	taskID := string(f.createTask(t, "ghosthost-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Acquire writes the session record stamped with the local
	// host. Manually rewrite Host to a foreign hostname to simulate
	// the cross-host case without standing up a second machine.
	first, err := Acquire(ctx, f.info, "ghosthost", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	verifySess := openVerifySessions(t, f)
	sess, rev, err := verifySess.Get(ctx, "ghosthost")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	sess.Host = "definitely-not-this-machine.invalid"
	if err := verifySess.update(ctx, sess, rev); err != nil {
		t.Fatalf("update with foreign host: %v", err)
	}

	_, err = Resume(ctx, f.info, "ghosthost", AcquireOpts{Hub: f.hub})
	if !errors.Is(err, ErrCrossHostOperation) {
		t.Fatalf("Resume: want ErrCrossHostOperation, got %v", err)
	}
}
