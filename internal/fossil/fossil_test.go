package fossil

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil"
)

// newTestManager constructs a Manager rooted at a fresh temp dir with
// a deterministic AgentID. t.Cleanup closes it.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	tmp := t.TempDir()
	cfg := Config{
		AgentID:      "test-agent",
		RepoPath:     filepath.Join(tmp, "repo.fossil"),
		CheckoutRoot: filepath.Join(tmp, "checkouts"),
	}
	m, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// commit is a test convenience wrapper: issues a single commit against
// the Manager and returns the resulting rev UUID.
func commit(
	t *testing.T, m *Manager, msg string, files ...File,
) string {
	t.Helper()
	rev, err := m.Commit(context.Background(), msg, files, "")
	if err != nil {
		t.Fatalf("Commit %q: %v", msg, err)
	}
	if rev == "" {
		t.Fatalf("Commit %q: empty rev", msg)
	}
	return rev
}

// testCheckin is a test-only helper that reaches into the Manager's
// unexported checkout to issue a commit with an explicit branch name.
// Production code does not need this capability (Manager.Commit is
// branch-agnostic); the helper exists solely so Merge tests can spin
// up a divergent feature branch. Requires the checkout to already be
// attached (call CreateCheckout first).
func testCheckin(
	t *testing.T, m *Manager, msg, branch string, files []File,
) string {
	t.Helper()
	if m.checkout == nil {
		t.Fatalf("testCheckin: checkout not attached; call CreateCheckout first")
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		full := filepath.Join(m.dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", full, err)
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			t.Fatalf("write %q: %v", full, err)
		}
		paths = append(paths, f.Path)
	}
	if _, err := m.checkout.Add(paths); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, uuid, err := m.checkout.Checkin(libfossil.CheckoutCommitOpts{
		Message: msg,
		User:    m.cfg.AgentID,
		Branch:  branch,
	})
	if err != nil {
		t.Fatalf("Checkin on branch %q: %v", branch, err)
	}
	return uuid
}

func TestOpen_CreatesRepo(t *testing.T) {
	tmp := t.TempDir()
	cfg := Config{
		AgentID:      "agent-a",
		RepoPath:     filepath.Join(tmp, "repo.fossil"),
		CheckoutRoot: filepath.Join(tmp, "checkouts"),
	}
	m, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if _, err := os.Stat(cfg.RepoPath); err != nil {
		t.Fatalf("repo file missing: %v", err)
	}
}

func TestOpen_ReattachesExisting(t *testing.T) {
	tmp := t.TempDir()
	cfg := Config{
		AgentID:      "agent-a",
		RepoPath:     filepath.Join(tmp, "repo.fossil"),
		CheckoutRoot: filepath.Join(tmp, "checkouts"),
	}
	m1, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := m1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	m2, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = m2.Close() })
}

func TestOpen_Close_Idempotent(t *testing.T) {
	m := newTestManager(t)
	if err := m.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestCreateCheckout_Fresh(t *testing.T) {
	m := newTestManager(t)
	// CreateCheckout requires a tip commit; land one via Manager.Commit.
	_ = commit(t, m, "init", File{
		Path:    "seed.txt",
		Content: []byte("seed\n"),
	})
	if err := m.CreateCheckout(context.Background()); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.dir, ".fslckout")); err != nil {
		t.Fatalf(".fslckout missing: %v", err)
	}
}

func TestCreateCheckout_Reopen(t *testing.T) {
	m := newTestManager(t)
	_ = commit(t, m, "init", File{
		Path:    "seed.txt",
		Content: []byte("seed\n"),
	})
	if err := m.CreateCheckout(context.Background()); err != nil {
		t.Fatalf("first CreateCheckout: %v", err)
	}
	first := m.checkout
	if err := m.CreateCheckout(context.Background()); err != nil {
		t.Fatalf("second CreateCheckout: %v", err)
	}
	if m.checkout != first {
		t.Fatalf("second CreateCheckout replaced checkout; want no-op")
	}
}

func TestCommit_InitialFile(t *testing.T) {
	m := newTestManager(t)
	rev := commit(t, m, "initial", File{
		Path:    "hello.txt",
		Content: []byte("hello\n"),
	})
	if len(rev) < 10 {
		t.Fatalf("rev %q looks too short for a UUID", rev)
	}
}

func TestCommit_MultipleFiles(t *testing.T) {
	m := newTestManager(t)
	rev := commit(t, m, "two files",
		File{Path: "a.txt", Content: []byte("a\n")},
		File{Path: "b.txt", Content: []byte("b\n")},
	)

	got, err := m.OpenFile(context.Background(), rev, "a.txt")
	if err != nil {
		t.Fatalf("OpenFile a.txt: %v", err)
	}
	if !bytes.Equal(got, []byte("a\n")) {
		t.Errorf("a.txt = %q, want %q", got, "a\n")
	}

	got, err = m.OpenFile(context.Background(), rev, "b.txt")
	if err != nil {
		t.Fatalf("OpenFile b.txt: %v", err)
	}
	if !bytes.Equal(got, []byte("b\n")) {
		t.Errorf("b.txt = %q, want %q", got, "b\n")
	}
}

func TestCommit_Modification(t *testing.T) {
	m := newTestManager(t)
	rev1 := commit(t, m, "v1", File{
		Path:    "doc.txt",
		Content: []byte("first\n"),
	})
	rev2 := commit(t, m, "v2", File{
		Path:    "doc.txt",
		Content: []byte("second\n"),
	})
	if rev1 == rev2 {
		t.Fatalf("two commits returned same rev %q", rev1)
	}
}

func TestCommit_NestedPaths(t *testing.T) {
	m := newTestManager(t)
	rev := commit(t, m, "nested", File{
		Path:    "sub/dir/file.txt",
		Content: []byte("nested\n"),
	})
	got, err := m.OpenFile(context.Background(), rev, "sub/dir/file.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if !bytes.Equal(got, []byte("nested\n")) {
		t.Errorf("got %q, want %q", got, "nested\n")
	}
}

func TestOpenFile_AtRev(t *testing.T) {
	m := newTestManager(t)
	rev := commit(t, m, "v1", File{
		Path:    "hello.txt",
		Content: []byte("hello\n"),
	})
	got, err := m.OpenFile(context.Background(), rev, "hello.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if !bytes.Equal(got, []byte("hello\n")) {
		t.Errorf("got %q, want %q", got, "hello\n")
	}
}

func TestOpenFile_AcrossRevs(t *testing.T) {
	m := newTestManager(t)
	rev1 := commit(t, m, "v1", File{
		Path:    "hello.txt",
		Content: []byte("old\n"),
	})
	rev2 := commit(t, m, "v2", File{
		Path:    "hello.txt",
		Content: []byte("new\n"),
	})

	got1, err := m.OpenFile(context.Background(), rev1, "hello.txt")
	if err != nil {
		t.Fatalf("OpenFile rev1: %v", err)
	}
	if !bytes.Equal(got1, []byte("old\n")) {
		t.Errorf("rev1 content = %q, want %q", got1, "old\n")
	}

	got2, err := m.OpenFile(context.Background(), rev2, "hello.txt")
	if err != nil {
		t.Fatalf("OpenFile rev2: %v", err)
	}
	if !bytes.Equal(got2, []byte("new\n")) {
		t.Errorf("rev2 content = %q, want %q", got2, "new\n")
	}
}

func TestOpenFile_NotInRev(t *testing.T) {
	m := newTestManager(t)
	rev := commit(t, m, "only hello", File{
		Path:    "hello.txt",
		Content: []byte("hi\n"),
	})
	_, err := m.OpenFile(context.Background(), rev, "missing.txt")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("got %v, want ErrFileNotFound", err)
	}
}

func TestOpenFile_UnknownRev(t *testing.T) {
	m := newTestManager(t)
	_ = commit(t, m, "init", File{
		Path:    "hello.txt",
		Content: []byte("hi\n"),
	})
	_, err := m.OpenFile(
		context.Background(),
		"0000000000000000000000000000000000000000",
		"hello.txt",
	)
	if !errors.Is(err, ErrRevNotFound) {
		t.Fatalf("got %v, want ErrRevNotFound", err)
	}
}

func TestDiff_Modification(t *testing.T) {
	m := newTestManager(t)
	rev1 := commit(t, m, "v1", File{
		Path:    "hello.txt",
		Content: []byte("hello\nworld\n"),
	})
	rev2 := commit(t, m, "v2", File{
		Path:    "hello.txt",
		Content: []byte("hello\nbrave new world\n"),
	})

	diff, err := m.Diff(context.Background(), rev1, rev2, "hello.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	s := string(diff)
	if !bytes.Contains(diff, []byte("-world")) {
		t.Errorf("diff missing -world line:\n%s", s)
	}
	if !bytes.Contains(diff, []byte("+brave new world")) {
		t.Errorf("diff missing +brave new world line:\n%s", s)
	}
}

func TestDiff_Identical(t *testing.T) {
	m := newTestManager(t)
	rev1 := commit(t, m, "v1",
		File{Path: "hello.txt", Content: []byte("same\n")},
		File{Path: "other.txt", Content: []byte("first\n")},
	)
	rev2 := commit(t, m, "touch other",
		File{Path: "hello.txt", Content: []byte("same\n")},
		File{Path: "other.txt", Content: []byte("second\n")},
	)

	diff, err := m.Diff(context.Background(), rev1, rev2, "hello.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(diff) != 0 {
		t.Fatalf("want empty diff for identical file, got %q", diff)
	}
}

func TestCheckout_Navigate(t *testing.T) {
	m := newTestManager(t)
	rev1 := commit(t, m, "v1", File{
		Path:    "hello.txt",
		Content: []byte("first\n"),
	})
	_ = commit(t, m, "v2", File{
		Path:    "hello.txt",
		Content: []byte("second\n"),
	})

	if err := m.CreateCheckout(context.Background()); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if err := m.Checkout(context.Background(), rev1); err != nil {
		t.Fatalf("Checkout rev1: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(m.dir, "hello.txt"))
	if err != nil {
		t.Fatalf("read on-disk: %v", err)
	}
	if !bytes.Equal(onDisk, []byte("first\n")) {
		t.Errorf("after Checkout(rev1), on-disk hello.txt = %q, want %q",
			onDisk, "first\n")
	}
}

func TestMerge_CleanMerge(t *testing.T) {
	m := newTestManager(t)
	// Prime trunk with the base commit.
	_ = commit(t, m, "base",
		File{Path: "a.txt", Content: []byte("base-a\n")},
		File{Path: "b.txt", Content: []byte("base-b\n")},
	)
	// Attach the checkout so testCheckin (which needs a checkout) can
	// put a commit on a named branch.
	if err := m.CreateCheckout(context.Background()); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	// Fork off a feature branch with an edit to a.txt.
	_ = testCheckin(t, m, "feature edits a", "feature", []File{
		{Path: "a.txt", Content: []byte("feature-a\n")},
		{Path: "b.txt", Content: []byte("base-b\n")},
	})
	// Extract back to trunk tip so the next testCheckin diverges from
	// the trunk baseline (not from the feature tip currently on disk).
	trunkTip, err := m.repo.BranchTip("trunk")
	if err != nil {
		t.Fatalf("BranchTip trunk: %v", err)
	}
	if err := m.checkout.Extract(
		trunkTip, libfossil.ExtractOpts{Force: true},
	); err != nil {
		t.Fatalf("Extract trunk tip: %v", err)
	}
	// Edit b.txt on trunk.
	_ = testCheckin(t, m, "trunk edits b", "trunk", []File{
		{Path: "a.txt", Content: []byte("base-a\n")},
		{Path: "b.txt", Content: []byte("trunk-b\n")},
	})

	uuid, err := m.Merge(
		context.Background(), "feature", "trunk", "merge feature",
	)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if uuid == "" {
		t.Fatal("merge returned empty uuid")
	}

	// Verify the merge pulled in both edits.
	aBytes, err := m.OpenFile(context.Background(), uuid, "a.txt")
	if err != nil {
		t.Fatalf("OpenFile a.txt: %v", err)
	}
	if !bytes.Equal(aBytes, []byte("feature-a\n")) {
		t.Errorf("a.txt = %q, want %q", aBytes, "feature-a\n")
	}
	bBytes, err := m.OpenFile(context.Background(), uuid, "b.txt")
	if err != nil {
		t.Fatalf("OpenFile b.txt: %v", err)
	}
	if !bytes.Equal(bBytes, []byte("trunk-b\n")) {
		t.Errorf("b.txt = %q, want %q", bBytes, "trunk-b\n")
	}
}

func TestMerge_BranchNotFound(t *testing.T) {
	m := newTestManager(t)
	_ = commit(t, m, "base", File{
		Path:    "a.txt",
		Content: []byte("base-a\n"),
	})

	_, err := m.Merge(
		context.Background(), "does-not-exist", "trunk", "merge",
	)
	if err == nil {
		t.Fatalf("Merge: want error for missing branch, got nil")
	}
}

// TestCheckout_NoCheckout_Errors verifies Checkout (navigation) errors
// with ErrNoCheckout before CreateCheckout has been called. Commit does
// NOT require a checkout in this implementation — see the package
// docstring — so there is no symmetric "Commit before CreateCheckout"
// error case.
func TestCheckout_NoCheckout_Errors(t *testing.T) {
	m := newTestManager(t)
	rev := commit(t, m, "init", File{
		Path:    "x.txt",
		Content: []byte("x\n"),
	})
	err := m.Checkout(context.Background(), rev)
	if !errors.Is(err, ErrNoCheckout) {
		t.Fatalf("got %v, want ErrNoCheckout", err)
	}
}

func TestCommit_AfterClose_Errors(t *testing.T) {
	m := newTestManager(t)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := m.Commit(context.Background(), "after close", []File{
		{Path: "x.txt", Content: []byte("x\n")},
	}, "")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

// TestWouldFork_NoCheckout proves WouldFork returns (false, nil) when
// no checkout is attached. A fresh manager cannot fork since it has no
// working-copy parent (ADR 0010 §4).
func TestWouldFork_NoCheckout(t *testing.T) {
	m := newTestManager(t)
	fork, err := m.WouldFork(context.Background())
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if fork {
		t.Fatalf("WouldFork on fresh manager: got true, want false")
	}
}

// TestWouldFork_SingleLeaf proves WouldFork returns false when the
// current branch has only one leaf (no sibling).
func TestWouldFork_SingleLeaf(t *testing.T) {
	m := newTestManager(t)
	_ = commit(t, m, "init", File{Path: "x.txt", Content: []byte("1\n")})
	if err := m.CreateCheckout(context.Background()); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	fork, err := m.WouldFork(context.Background())
	if err != nil {
		t.Fatalf("WouldFork: %v", err)
	}
	if fork {
		t.Fatalf("WouldFork with single leaf: got true, want false")
	}
}

// TestWouldFork_AfterClose proves WouldFork returns ErrClosed when
// invoked on a torn-down Manager.
func TestWouldFork_AfterClose(t *testing.T) {
	m := newTestManager(t)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := m.WouldFork(context.Background())
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("WouldFork after close: got %v, want ErrClosed", err)
	}
}

// TestWouldFork_TwoManagersSharedRepo pins the fork semantics used by
// ADR 0010 §4: two Managers on the same shared repo with distinct
// CheckoutRoots trade commits. After A commits and B commits in
// sequence, A's checkout (attached at A1) must see the sibling leaf
// B1 and report WouldFork=true — that's the condition coord.Commit
// reads to decide between trunk and fork-branch placement.
func TestWouldFork_TwoManagersSharedRepo(t *testing.T) {
	tmp := t.TempDir()
	shared := filepath.Join(tmp, "shared.fossil")
	mkMgr := func(id string) *Manager {
		m, err := Open(context.Background(), Config{
			AgentID:      id,
			RepoPath:     shared,
			CheckoutRoot: filepath.Join(tmp, id+"-checkouts"),
		})
		if err != nil {
			t.Fatalf("Open(%s): %v", id, err)
		}
		t.Cleanup(func() { _ = m.Close() })
		return m
	}
	mA := mkMgr("A")
	mB := mkMgr("B")
	ctx := context.Background()

	// A commits first; checkout not yet attached.
	if _, err := mA.Commit(ctx, "a1", []File{
		{Path: "a.go", Content: []byte("a1\n")},
	}, ""); err != nil {
		t.Fatalf("A commit 1: %v", err)
	}
	// Attach A's checkout so fork detection has a working-copy anchor.
	if err := mA.CreateCheckout(ctx); err != nil {
		t.Fatalf("A CreateCheckout: %v", err)
	}
	// Attach B's checkout after A's tip is visible.
	if err := mB.CreateCheckout(ctx); err != nil {
		t.Fatalf("B CreateCheckout: %v", err)
	}
	// Snapshot B's checkout at A's tip (the only rev so far).
	// Leaves on trunk = [A1]; WouldFork on A = false.
	forkA, err := mA.WouldFork(ctx)
	if err != nil {
		t.Fatalf("A WouldFork after A1: %v", err)
	}
	if forkA {
		t.Fatalf("A WouldFork after A1: got true, want false")
	}

	// B commits; this should advance trunk past A1 (B's commit
	// references A1 as parent since tipRID returns A1). Now A1
	// becomes internal; B1 is the only leaf. No fork yet.
	if _, err := mB.Commit(ctx, "b1", []File{
		{Path: "b.go", Content: []byte("b1\n")},
	}, ""); err != nil {
		t.Fatalf("B commit 1: %v", err)
	}
	// A's WouldFork still sees A's checkout at A1. Leaves on trunk
	// after B1 = [B1]. A1 is internal. A's rid != any leaf rid,
	// so WouldFork returns true (sibling leaf B1 exists).
	forkA, err = mA.WouldFork(ctx)
	if err != nil {
		t.Fatalf("A WouldFork after B1: %v", err)
	}
	if !forkA {
		t.Fatalf("A WouldFork after B1: got false, want true")
	}
}

// TestCommit_WithBranch_PlacesOnBranch proves that passing a non-empty
// branch arg to Commit results in the new rev being placed on that
// named branch (ADR 0010 §5 fork-on-conflict composition primitive).
func TestCommit_WithBranch_PlacesOnBranch(t *testing.T) {
	m := newTestManager(t)
	// Land a baseline on trunk so the forked commit has a parent.
	_ = commit(t, m, "base", File{Path: "x.txt", Content: []byte("1\n")})
	if err := m.CreateCheckout(context.Background()); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	// Commit again with an explicit branch name.
	branch := "agent-a-task1-12345"
	rev, err := m.Commit(
		context.Background(), "forked",
		[]File{{Path: "x.txt", Content: []byte("2\n")}},
		branch,
	)
	if err != nil {
		t.Fatalf("Commit with branch: %v", err)
	}
	if rev == "" {
		t.Fatal("Commit with branch: empty rev")
	}
	// Confirm the named branch resolves to the new tip.
	tipRID, err := m.repo.BranchTip(branch)
	if err != nil {
		t.Fatalf("BranchTip %q: %v", branch, err)
	}
	if tipRID == 0 {
		t.Fatalf("BranchTip %q: got 0 rid", branch)
	}
}
