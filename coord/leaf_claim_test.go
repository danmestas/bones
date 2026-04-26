// coord/leaf_claim_test.go
package coord

import (
	"context"
	"testing"
	"time"
)

// TestLeaf_Claim validates that Leaf.Claim opens a task on the leaf's
// substrate Coord and acquires it. The returned Claim carries the
// release closure so the caller can rel() at end-of-scope.
func TestLeaf_Claim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, t.TempDir(), "slot-A",
		hub.LeafUpstream(), hub.NATSURL(), hub.HTTPAddr())
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	// Claim a task that the leaf opens on its own Coord.
	taskID, err := l.OpenTask(ctx, "leaf-claim-test", []string{"/slot-A/x.txt"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if cl == nil {
		t.Fatalf("Claim: nil claim returned")
	}
	if cl.TaskID() != taskID {
		t.Fatalf("Claim.TaskID: got %v want %v", cl.TaskID(), taskID)
	}
}
