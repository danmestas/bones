// coord/leaf_compact_test.go
package coord

import (
	"context"
	"testing"
	"time"
)

// stubLeafSummarizer satisfies Summarizer for Leaf.Compact tests; returns
// a deterministic string keyed off the input TaskID so assertions can
// distinguish per-task summaries.
type stubLeafSummarizer struct{}

func (stubLeafSummarizer) Summarize(_ context.Context, in CompactInput) (string, error) {
	return "stub summary for " + string(in.TaskID), nil
}

// TestLeaf_Compact_WritesArtifact validates that Leaf.Compact (a)
// resolves eligible closed tasks via the leaf's substrate Coord, (b)
// writes the artifact through l.agent.Repo() (the only fossil handle
// in the process), and (c) calls SyncNow so the artifact propagates to
// the hub. The test asserts on the result shape; deeper read-side
// inspection lives in the architectural commit_test.go suite.
func TestLeaf_Compact_WritesArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, LeafConfig{Hub: hub, Workdir: t.TempDir(), SlotID: "slot-K"})
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	// Open and immediately close a task so it is eligible for compaction.
	taskID, err := l.OpenTask(ctx, "compact-target", []string{"/slot-K/c.txt"})
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

	res, err := l.Compact(ctx, CompactOptions{
		MinAge:     0,
		Limit:      4,
		Summarizer: stubLeafSummarizer{},
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("Compact: tasks=%d want=1", len(res.Tasks))
	}
	got := res.Tasks[0]
	if got.TaskID != taskID {
		t.Fatalf("Compact: TaskID=%q want=%q", got.TaskID, taskID)
	}
	if got.Rev == "" {
		t.Fatalf("Compact: empty Rev")
	}
	if got.CompactLevel != 1 {
		t.Fatalf("Compact: CompactLevel=%d want=1", got.CompactLevel)
	}
}
