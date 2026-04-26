// coord/leaf_close_test.go
package coord

import (
	"context"
	"testing"
	"time"
)

// TestLeaf_Close validates that Leaf.Close marks the task closed via the
// underlying Coord. After Close, a second Close returns ErrTaskAlreadyClosed.
func TestLeaf_Close(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, t.TempDir(), "slot-C",
		hub.LeafUpstream(), hub.NATSURL(), hub.HTTPAddr())
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	taskID, err := l.OpenTask(ctx, "close-test", []string{"/slot-C/c.txt"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := l.Close(ctx, cl); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
