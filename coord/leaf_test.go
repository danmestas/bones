// coord/leaf_test.go
package coord

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestLeaf_OpenStopTipWT validates the Leaf lifecycle and read-side
// accessors. Leaf opens against a Hub (in-process), exposes Tip()
// (empty on a fresh repo) and WT() (slot worktree path), and Stop
// is clean.
func TestLeaf_OpenStopTipWT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hubDir := t.TempDir()
	hub, err := OpenHub(ctx, hubDir, freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	leafDir := t.TempDir()
	slotID := "slot-0"
	l, err := OpenLeaf(ctx, leafDir, slotID, hub.NATSURL(), hub.HTTPAddr())
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	tip, err := l.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip != "" {
		t.Fatalf("Tip on fresh leaf: got %q, want empty", tip)
	}
	wantWT := filepath.Join(leafDir, slotID, "wt")
	if l.WT() != wantWT {
		t.Fatalf("WT: got %q want %q", l.WT(), wantWT)
	}
	if err := l.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
