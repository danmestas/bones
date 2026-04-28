// coord/leaf_config_test.go
package coord

import (
	"context"
	"testing"
	"time"
)

// TestLeafConfig_Metadata verifies that Metadata key=value pairs set on
// LeafConfig are accessible via Leaf.Metadata after OpenLeaf.
func TestLeafConfig_Metadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, LeafConfig{
		Hub:     hub,
		Workdir: t.TempDir(),
		SlotID:  "slot-meta",
		Metadata: map[string]string{
			"run_id": "run-42",
			"model":  "sonnet",
		},
	})
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	if got := l.Metadata("run_id"); got != "run-42" {
		t.Errorf("Metadata(run_id): got %q, want %q", got, "run-42")
	}
	if got := l.Metadata("model"); got != "sonnet" {
		t.Errorf("Metadata(model): got %q, want %q", got, "sonnet")
	}
	if got := l.Metadata("missing"); got != "" {
		t.Errorf("Metadata(missing): got %q, want empty", got)
	}
}

// TestLeafConfig_Metadatanil verifies that Leaf.Metadata returns "" when
// LeafConfig.Metadata is nil (most callers don't set it).
func TestLeafConfig_MetadataNil(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, LeafConfig{
		Hub:     hub,
		Workdir: t.TempDir(),
		SlotID:  "slot-nometa",
	})
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	if got := l.Metadata("any"); got != "" {
		t.Errorf("Metadata on nil map: got %q, want empty", got)
	}
}

// TestLeafConfig_ClaimTTL verifies that a non-zero ClaimTTL is used
// instead of the substrate's HoldTTLDefault when Leaf.Claim is called.
// We confirm by opening a task with a short TTL — if the TTL were ignored,
// the substrate HoldTTLDefault (30s) would be used, but we can only verify
// the call succeeds (hold is acquired) since TTL assertions require
// substrate timing access not exposed publicly.
func TestLeafConfig_ClaimTTL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, LeafConfig{
		Hub:      hub,
		Workdir:  t.TempDir(),
		SlotID:   "slot-ttl",
		ClaimTTL: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	taskID, err := l.OpenTask(ctx, "ttl-test", []string{"/slot-ttl/x.txt"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		t.Fatalf("Claim with ClaimTTL=10s: %v", err)
	}
	if cl == nil || cl.TaskID() != taskID {
		t.Fatalf("Claim: unexpected claim result")
	}
	// Release so teardown is clean.
	if err := cl.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestLeafConfig_FossilUser verifies that a commit authored after setting
// FossilUser records the custom user rather than SlotID.
func TestLeafConfig_FossilUser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, LeafConfig{
		Hub:        hub,
		Workdir:    t.TempDir(),
		SlotID:     "slot-fuser",
		FossilUser: "stable-identity",
	})
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	taskID, err := l.OpenTask(ctx, "fossil-user-test", []string{"/slot-fuser/a.txt"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	_, err = l.Commit(ctx, cl, []File{{Path: "/slot-fuser/a.txt", Content: []byte("hi")}})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// The commit should record fossilUser as the author. Verify by reading
	// the fossil event user column via Tip + SQL.
	repo := l.agent.Repo()
	var user string
	err = repo.DB().QueryRow(`
		SELECT e.user FROM leaf lf
		JOIN event e ON e.objid=lf.rid
		WHERE e.type='ci'
		ORDER BY e.mtime DESC, lf.rid DESC LIMIT 1
	`).Scan(&user)
	if err != nil {
		t.Fatalf("query commit user: %v", err)
	}
	if user != "stable-identity" {
		t.Errorf("commit user: got %q, want %q", user, "stable-identity")
	}
}
