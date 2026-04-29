// coord/leaf_commit_test.go
package coord

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/libfossil"
)

// TestLeaf_CommitWritesAndSyncs validates that Leaf.Commit (a) writes the
// file via agent.Repo(), (b) calls SyncNow, (c) returns nil on
// disjoint-slot success, and (d) the commit propagates to the hub's
// fossil repo.
//
// The hub-propagation check is the architectural point of this test:
// "commits propagate via leaf.Agent's sync". Without it the test only
// proves the local write, not the cross-process sync flow.
func TestLeaf_CommitWritesAndSyncs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hubDir := t.TempDir()
	hub, err := OpenHub(ctx, hubDir, freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, LeafConfig{Hub: hub, Workdir: t.TempDir(), SlotID: "slot-A"})
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	taskID, err := l.OpenTask(ctx, "commit-test", []string{"/slot-A/file.txt"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Cleanup(func() { _ = cl.Release() })

	uuid, err := l.Commit(ctx, cl, []File{
		{Path: "/slot-A/file.txt", Content: []byte("hello")},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if uuid == "" {
		t.Fatalf("Commit: empty UUID returned")
	}

	tip, err := l.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip == "" {
		t.Fatalf("Tip after commit: empty")
	}
	if tip != uuid {
		t.Fatalf("Tip != Commit UUID: tip=%q uuid=%q", tip, uuid)
	}

	// Hub-propagation check: open a second read-only handle to
	// hub.fossil and confirm the manifest is present. A separate
	// SQLite handle is fine for read-only inspection in tests; the
	// running hub.agent retains write ownership but the SQLite WAL
	// permits concurrent readers.
	if err := assertCommitOnHub(t, hubDir, uuid); err != nil {
		t.Fatalf("hub propagation: %v", err)
	}
}

// TestLeaf_CommitDivergenceBranch exercises the parent != "" branch of
// Leaf.Commit's post-sync divergence check. The first commit hits the
// parent == "" branch (fresh repo); the second commit on the same leaf
// has a non-empty parent, so the divergence check compares
// post-tip != parent to assert the tip advanced. Both commits must
// succeed and the tip must change between them.
func TestLeaf_CommitDivergenceBranch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hubDir := t.TempDir()
	hub, err := OpenHub(ctx, hubDir, freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, LeafConfig{Hub: hub, Workdir: t.TempDir(), SlotID: "slot-B"})
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	// First commit — parent == "" branch.
	taskA, err := l.OpenTask(ctx, "first", []string{"/slot-B/a.txt"})
	if err != nil {
		t.Fatalf("OpenTask 1: %v", err)
	}
	clA, err := l.Claim(ctx, taskA)
	if err != nil {
		t.Fatalf("Claim 1: %v", err)
	}
	uuidA, err := l.Commit(ctx, clA, []File{
		{Path: "/slot-B/a.txt", Content: []byte("first")},
	})
	if err != nil {
		t.Fatalf("Commit 1: %v", err)
	}
	if err := clA.Release(); err != nil {
		t.Fatalf("Release 1: %v", err)
	}
	if uuidA == "" {
		t.Fatalf("Commit 1: empty UUID")
	}

	// Second commit — parent != "" branch. The pre-tip lookup now
	// returns uuidA, so the post-sync divergence check executes the
	// `parent != "" && post == parent` comparison.
	taskB, err := l.OpenTask(ctx, "second", []string{"/slot-B/b.txt"})
	if err != nil {
		t.Fatalf("OpenTask 2: %v", err)
	}
	clB, err := l.Claim(ctx, taskB)
	if err != nil {
		t.Fatalf("Claim 2: %v", err)
	}
	uuidB, err := l.Commit(ctx, clB, []File{
		{Path: "/slot-B/b.txt", Content: []byte("second")},
	})
	if err != nil {
		t.Fatalf("Commit 2 (parent != \"\"): %v", err)
	}
	if err := clB.Release(); err != nil {
		t.Fatalf("Release 2: %v", err)
	}
	if uuidB == "" {
		t.Fatalf("Commit 2: empty UUID")
	}
	if uuidA == uuidB {
		t.Fatalf("tip did not advance: uuidA == uuidB == %q", uuidA)
	}

	// Confirm the leaf's tip advanced to the second commit.
	tip, err := l.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip != uuidB {
		t.Fatalf("Tip after Commit 2: got %q want %q", tip, uuidB)
	}
}

// TestLeaf_CommitFileNameOverride pins the contract that File.Name,
// when set, is the libfossil file name in the resulting manifest.
// Path remains the holds-gate key (must be absolute), but Name is
// what shows up under "F" cards / fossil ls. Without this override
// every slot-style caller would see commits land at
// "<workspace-prefix>/<rel>" inside the repo because Leaf.Commit
// would derive the file name by stripping a single leading slash off
// the absolute hold path.
func TestLeaf_CommitFileNameOverride(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hubDir := t.TempDir()
	hub, err := OpenHub(ctx, hubDir, freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, LeafConfig{Hub: hub, Workdir: t.TempDir(), SlotID: "slot-N"})
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	// Path mimics a swarm-style absolute holds key
	// ("/tmp/ws/.bones/swarm/slot-N/wt/src/foo.go" — but here we just
	// use a path the holds-gate accepts). Name is the repo-relative
	// path callers want libfossil to record.
	holdPath := "/slot-N/abs/with/prefix/src/foo.go"
	repoName := "src/foo.go"

	taskID, err := l.OpenTask(ctx, "name-override", []string{holdPath})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Cleanup(func() { _ = cl.Release() })

	uuid, err := l.Commit(ctx, cl, []File{
		{Path: holdPath, Name: repoName, Content: []byte("body")},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	repo := l.agent.Repo()
	rid, err := repo.ResolveVersion(uuid)
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	entries, err := repo.ListFiles(rid)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListFiles: got %d entries, want 1: %+v", len(entries), entries)
	}
	if entries[0].Name != repoName {
		t.Fatalf(
			"file name in manifest: got %q, want %q (Name override should win over Path-derived)",
			entries[0].Name, repoName,
		)
	}
}

// TestLeaf_CommitAutosyncLinearizes pins the trunk-based contract:
// when two leaves share a hub and both run with Autosync=true, the
// second leaf's commit lists the first leaf's commit as its parent —
// i.e. trunk advances linearly instead of forking into parallel
// leaves that fan-in must collapse later.
//
// Without autosync each leaf's local view of "trunk tip" is the seed
// (frozen at clone time), so both Commits would list the seed as
// parent and the hub would carry two open leaves on trunk. With
// autosync the second leaf pulls the first leaf's already-pushed
// commit before resolving BranchTip, so its parent is the first
// commit's UUID.
func TestLeaf_CommitAutosyncLinearizes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	hubDir := t.TempDir()
	hub, err := OpenHub(ctx, hubDir, freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	openLeaf := func(slot string) *Leaf {
		l, err := OpenLeaf(ctx, LeafConfig{
			Hub:      hub,
			Workdir:  t.TempDir(),
			SlotID:   slot,
			Autosync: true,
		})
		if err != nil {
			t.Fatalf("OpenLeaf %s: %v", slot, err)
		}
		t.Cleanup(func() { _ = l.Stop() })
		return l
	}
	leafA := openLeaf("slot-A")
	leafB := openLeaf("slot-B")

	commitOnce := func(l *Leaf, holdPath, repoName string, body []byte) string {
		t.Helper()
		taskID, err := l.OpenTask(ctx, "auto-"+repoName, []string{holdPath})
		if err != nil {
			t.Fatalf("OpenTask: %v", err)
		}
		cl, err := l.Claim(ctx, taskID)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		uuid, err := l.Commit(ctx, cl, []File{
			{Path: holdPath, Name: repoName, Content: body},
		})
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := cl.Release(); err != nil {
			t.Fatalf("Release: %v", err)
		}
		return uuid
	}

	// Slot-A commits first. With autosync, this pulls (no-op on a
	// fresh hub) and commits on the seed.
	uuidA := commitOnce(leafA, "/slot-A/file.txt", "a/file.txt", []byte("from A"))

	// Wait for slot-A's commit to be visible on the hub before slot-B
	// commits — otherwise slot-B's pull may run before A's push lands
	// and the test would race against the in-process sync goroutine.
	if err := assertCommitOnHub(t, hubDir, uuidA); err != nil {
		t.Fatalf("hub propagation A: %v", err)
	}

	// Slot-B commits next. With autosync, slot-B pulls and now sees
	// uuidA on hub trunk, so its commit's parent should be uuidA.
	uuidB := commitOnce(leafB, "/slot-B/file.txt", "b/file.txt", []byte("from B"))

	parentB, err := parentUUID(leafB.agent.Repo(), uuidB)
	if err != nil {
		t.Fatalf("parentUUID(uuidB): %v", err)
	}
	if parentB != uuidA {
		t.Fatalf(
			"autosync did not linearize trunk: uuidB parent = %q, want uuidA %q",
			parentB, uuidA,
		)
	}
}

// parentUUID returns the parent commit UUID of the manifest with the
// given UUID, by joining blob → plink → blob in fossil's schema. Used
// to assert pre-commit pull made the prior commit visible.
func parentUUID(repo *libfossil.Repo, child string) (string, error) {
	var parent string
	err := repo.DB().QueryRow(`
		SELECT pblob.uuid FROM blob AS pblob
		JOIN plink ON plink.pid = pblob.rid
		JOIN blob AS cblob ON cblob.rid = plink.cid
		WHERE cblob.uuid = ?
	`, child).Scan(&parent)
	return parent, err
}

// assertCommitOnHub opens hub.fossil read-only and checks that the
// manifest with the given UUID exists in the blob table. The hub's
// running agent owns the write side; a separate read-only handle is
// safe because SQLite WAL permits concurrent readers.
func assertCommitOnHub(t *testing.T, hubDir, uuid string) error {
	t.Helper()
	repoPath := filepath.Join(hubDir, "hub.fossil")
	// Poll briefly: SyncNow returns once the push completes, but the
	// hub's HandleSync runs on the receiving side — there can be a
	// short window before the manifest is visible in the hub's blob
	// table. 2s is generous for an in-process hub.
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		r, err := libfossil.Open(repoPath)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var n int
		err = r.DB().QueryRow(
			`SELECT COUNT(*) FROM blob WHERE uuid = ?`, uuid,
		).Scan(&n)
		_ = r.Close()
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if n == 1 {
			return nil
		}
		lastErr = nil
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		return lastErr
	}
	return &commitNotOnHubErr{uuid: uuid}
}

type commitNotOnHubErr struct{ uuid string }

func (e *commitNotOnHubErr) Error() string {
	return "commit " + e.uuid + " not visible in hub.fossil after 2s"
}
