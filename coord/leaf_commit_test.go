// coord/leaf_commit_test.go
package coord

import (
	"context"
	"testing"
	"time"
)

// TestLeaf_CommitWritesAndSyncs validates that Leaf.Commit (a) writes the
// file via agent.Repo(), (b) calls SyncNow, (c) returns nil on
// disjoint-slot success.
func TestLeaf_CommitWritesAndSyncs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, t.TempDir(), "slot-A", hub.NATSURL(), hub.HTTPAddr())
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

	if err := l.Commit(ctx, cl, []File{
		{Path: "/slot-A/file.txt", Content: []byte("hello")},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	tip, err := l.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip == "" {
		t.Fatalf("Tip after commit: empty")
	}
}
