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
// (writing hub.fossil under .bones) and returns the
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
	orch := filepath.Join(dir, ".bones")
	if err := os.MkdirAll(orch, 0o755); err != nil {
		t.Fatalf("mkdir .bones: %v", err)
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
// `.bones/hub.fossil` MUST cause Acquire to return
// ErrWorkspaceNotBootstrapped without attempting any other work.
// The error string MUST NOT contain "run `bones up`" — that
// guidance is for orchestrators, not leaves.
func TestAcquire_RefusesWithoutHubFossil(t *testing.T) {
	dir := t.TempDir() // no .bones/hub.fossil
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

// TestAcquire_OpensWorktreeCheckout pins the fix for #117:
// Acquire must leave the slot worktree as a real fossil checkout
// containing the trunk's files, not a bare empty directory. The
// presence of `.fslckout` proves the libfossil CreateCheckout step
// ran inside Acquire; the readability of the seed file proves the
// slot can read prior phases' committed artifacts off disk without
// running a separate pull/sync verb.
func TestAcquire_OpensWorktreeCheckout(t *testing.T) {
	f := newLeaseFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Seed the hub so it mirrors a post-`bones up` state. Slot A
	// acquires, commits a marker file, releases — this advances the
	// hub's trunk.
	seedHold := filepath.Join(f.dir, "seed", "seed.txt")
	seedTask := string(f.createTask(t, "join117-seed-task", seedHold))
	seedLease, err := Acquire(ctx, f.info, "seed", seedTask, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("seed Acquire: %v", err)
	}
	if err := seedLease.Release(ctx); err != nil {
		t.Fatalf("seed Release: %v", err)
	}
	seedResumed, err := Resume(ctx, f.info, "seed", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("seed Resume: %v", err)
	}
	seedFiles := []coord.File{{
		Path:    wspath.Must(seedHold),
		Name:    "seed.txt",
		Content: []byte("phase1-artifact"),
	}}
	if _, err := seedResumed.Commit(ctx, "seed for #117 test", seedFiles); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}
	if err := seedResumed.Release(ctx); err != nil {
		t.Fatalf("seed final Release: %v", err)
	}

	// Now slot B acquires against the seeded hub. The wt must contain
	// both .fslckout and the seeded file readable from disk.
	holdPath := filepath.Join(f.dir, "join117", "hello.txt")
	taskID := string(f.createTask(t, "join117-task-1", holdPath))
	lease, err := Acquire(ctx, f.info, "join117", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release(ctx) })

	wt := lease.WT()
	if wt == "" {
		t.Fatalf("WT: empty")
	}
	if _, err := os.Stat(filepath.Join(wt, ".fslckout")); err != nil {
		t.Fatalf(".fslckout missing in worktree (Acquire did not open a checkout): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "seed.txt"))
	if err != nil {
		t.Fatalf("seed.txt missing in slot B worktree: %v", err)
	}
	if string(got) != "phase1-artifact" {
		t.Errorf("seed.txt content: got %q want %q", got, "phase1-artifact")
	}
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
	// This test pins the cleanup contract (record + pid file removed,
	// slot reusable), not the artifact contract — bypass the
	// "no commit since join" precondition with NoArtifact so the
	// cleanup path is what's exercised.
	if err := resumed.Close(ctx, CloseOpts{
		CloseTaskOnSuccess: true,
		NoArtifact:         "test: covers cleanup path",
	}); err != nil {
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

// TestClose_RefusesSuccessWithoutCommit pins the artifact-contract
// precondition: a slot that was joined but never committed must not
// be allowed to close --result=success silently. The substrate
// refuses the silent-bypass shape so the audit trail bones promises
// (every successful slot leaves a commit) holds structurally rather
// than by agent politeness.
func TestClose_RefusesSuccessWithoutCommit(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "noartifact", "hello.txt")
	taskID := string(f.createTask(t, "noartifact-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "noartifact", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	resumed, err := Resume(ctx, f.info, "noartifact", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	closeErr := resumed.Close(ctx, CloseOpts{CloseTaskOnSuccess: true})
	if !errors.Is(closeErr, ErrCloseRequiresArtifact) {
		t.Fatalf("Close: want ErrCloseRequiresArtifact, got %v", closeErr)
	}

	// The lease should still be usable after a refused close — the
	// caller's recovery path is to commit, then retry close.
	if err := resumed.Close(ctx, CloseOpts{
		CloseTaskOnSuccess: true,
		NoArtifact:         "test-bypass",
	}); err != nil {
		t.Fatalf("Close with NoArtifact bypass: %v", err)
	}
}

// TestClose_RemovesWTOnSuccess pins the fix for #120: a successful
// close must remove the per-slot worktree directory so it does not
// accumulate across cycles. KV record + pid file removal are
// covered by TestClose_DeletesRecordAndPidFile; this test is
// scoped to the wt path.
func TestClose_RemovesWTOnSuccess(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "wtremove", "hello.txt")
	taskID := string(f.createTask(t, "wtremove-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "wtremove", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	wt := SlotWorktree(f.dir, "wtremove")
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("pre-Close wt missing: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release after Acquire: %v", err)
	}

	resumed, err := Resume(ctx, f.info, "wtremove", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := resumed.Close(ctx, CloseOpts{
		CloseTaskOnSuccess: true,
		NoArtifact:         "test: covers wt-removal path",
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("wt should be gone after success Close: stat err=%v", err)
	}
}

// TestClose_RetainsWTOnFail pins the asymmetry: a failed close must
// leave the per-slot worktree on disk so the operator can inspect
// what the slot left behind. fork results follow the same retention
// rule by virtue of CloseTaskOnSuccess=false.
func TestClose_RetainsWTOnFail(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "wtfail", "hello.txt")
	taskID := string(f.createTask(t, "wtfail-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "wtfail", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	resumed, err := Resume(ctx, f.info, "wtfail", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := resumed.Close(ctx, CloseOpts{CloseTaskOnSuccess: false}); err != nil {
		t.Fatalf("Close fail: %v", err)
	}
	wt := SlotWorktree(f.dir, "wtfail")
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("wt should be retained after fail Close: %v", err)
	}
}

// TestClose_KeepWTOnSuccess pins the forensics opt-out: a successful
// close with CloseOpts.KeepWT=true must retain the worktree even
// when CloseTaskOnSuccess=true.
func TestClose_KeepWTOnSuccess(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "wtkeep", "hello.txt")
	taskID := string(f.createTask(t, "wtkeep-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "wtkeep", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	resumed, err := Resume(ctx, f.info, "wtkeep", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := resumed.Close(ctx, CloseOpts{
		CloseTaskOnSuccess: true,
		NoArtifact:         "test: covers keep-wt path",
		KeepWT:             true,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wt := SlotWorktree(f.dir, "wtkeep")
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("wt should be retained when KeepWT=true: %v", err)
	}
}

// TestClose_IdempotentMissingWT pins the close-converges contract
// for the wt-removal step: a close on a slot whose wt was already
// removed (e.g. by a crashed prior close, or manual cleanup) must
// still succeed without error.
func TestClose_IdempotentMissingWT(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "wtmissing", "hello.txt")
	taskID := string(f.createTask(t, "wtmissing-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "wtmissing", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Pre-remove the wt so Close must converge through a missing-dir
	// state rather than treating it as an error.
	wt := SlotWorktree(f.dir, "wtmissing")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("pre-remove wt: %v", err)
	}
	resumed, err := Resume(ctx, f.info, "wtmissing", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := resumed.Close(ctx, CloseOpts{
		CloseTaskOnSuccess: true,
		NoArtifact:         "test: covers idempotent missing-wt",
	}); err != nil {
		t.Fatalf("Close (wt already missing): %v", err)
	}
}

// TestClose_AllowsFailWithoutCommit covers the asymmetry: the
// precondition gates only --result=success. A failed or forked
// close has no artifact contract — the slot didn't claim to have
// produced something — so the precondition must not engage.
func TestClose_AllowsFailWithoutCommit(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "failclose", "hello.txt")
	taskID := string(f.createTask(t, "failclose-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "failclose", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	resumed, err := Resume(ctx, f.info, "failclose", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := resumed.Close(ctx, CloseOpts{CloseTaskOnSuccess: false}); err != nil {
		t.Fatalf("Close fail: %v", err)
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
