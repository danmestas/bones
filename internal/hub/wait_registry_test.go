package hub

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/registry"
)

// TestWaitForRegistry_ReturnsImmediatelyWhenAlreadyRecorded pins the
// happy-no-op path: if the registry already has an entry whose
// HubPID matches childPID at call time, waitForRegistry returns
// without polling.
func TestWaitForRegistry_ReturnsImmediatelyWhenAlreadyRecorded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	// Use the test process's own pid so registry.Read's self-pruning
	// of dead-pid entries doesn't delete the entry between Write and
	// the first poll (real production callers always pass a live pid).
	livePID := os.Getpid()
	if err := registry.Write(registry.Entry{
		Cwd:       root,
		Name:      "test",
		HubPID:    livePID,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("registry.Write: %v", err)
	}

	start := time.Now()
	if err := waitForRegistry(root, livePID, time.Second); err != nil {
		t.Fatalf("waitForRegistry: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("waitForRegistry took %v on already-recorded entry; "+
			"should have returned without polling", elapsed)
	}
}

// TestWaitForRegistry_TimesOutWhenNotRecorded pins that a registry
// that never gets the right entry causes waitForRegistry to return
// a timeout error mentioning the expected pid. Pre-#355 the parent
// CLI verb didn't wait at all; this test guards against a regression
// that would silently re-introduce the gap.
func TestWaitForRegistry_TimesOutWhenNotRecorded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()

	start := time.Now()
	err := waitForRegistry(root, 99999, 200*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("waitForRegistry returned in %v; expected at least 200ms timeout",
			elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("waitForRegistry took %v on missing entry; expected ~200ms timeout",
			elapsed)
	}
	if !strings.Contains(err.Error(), "99999") {
		t.Errorf("error should mention expected pid 99999: %v", err)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should mention timeout: %v", err)
	}
}

// TestWaitForRegistry_PollsUntilEntryAppears simulates the actual
// race the fix is closing: another goroutine writes the registry
// entry mid-poll. waitForRegistry must observe the write and return
// without hitting the timeout. This is the integration-shape test —
// it pins the operator-visible contract that lazy-start blocks until
// the registry catches up.
func TestWaitForRegistry_PollsUntilEntryAppears(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()

	// Write the entry from a goroutine after a short delay, simulating
	// the real-world race: child is still in runTaskRecovery while
	// the parent waits. Use the test's own pid so the entry survives
	// registry.Read's dead-pid self-pruning.
	livePID := os.Getpid()
	const writeDelay = 100 * time.Millisecond
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(writeDelay)
		_ = registry.Write(registry.Entry{
			Cwd:       root,
			Name:      "test",
			HubPID:    livePID,
			StartedAt: time.Now().UTC(),
		})
	}()

	start := time.Now()
	err := waitForRegistry(root, livePID, 2*time.Second)
	elapsed := time.Since(start)
	wg.Wait()

	if err != nil {
		t.Fatalf("waitForRegistry: %v", err)
	}
	if elapsed < writeDelay {
		t.Errorf("waitForRegistry returned in %v before writeDelay %v — "+
			"didn't actually wait for the entry", elapsed, writeDelay)
	}
	if elapsed > writeDelay+500*time.Millisecond {
		t.Errorf("waitForRegistry took %v but entry was written at %v — "+
			"poll loop may not be reacting promptly", elapsed, writeDelay)
	}
}

// TestWaitForRegistry_RejectsMismatchedPID pins that a registry
// entry whose HubPID is a DIFFERENT pid (e.g. a stale one from a
// crashed prior hub) is treated as "not ready yet" and the loop
// keeps polling — not a false-positive success.
func TestWaitForRegistry_RejectsMismatchedPID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	// Stale entry from some other pid.
	if err := registry.Write(registry.Entry{
		Cwd:       root,
		Name:      "test",
		HubPID:    11111,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("registry.Write: %v", err)
	}

	err := waitForRegistry(root, 22222, 200*time.Millisecond)
	if err == nil {
		t.Fatal("waitForRegistry returned nil on mismatched pid; " +
			"should have polled past the stale entry until timeout")
	}
}
