package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	edgehub "github.com/danmestas/EdgeSync/hub"
)

// TestWaitOrTimeout_DoneClosed asserts the bounded-wait helper returns
// nil when its done channel closes before the timeout expires. This is
// the happy path for runForeground's drain wait — NATS shutdown completes
// in time and the hub exits cleanly.
func TestWaitOrTimeout_DoneClosed(t *testing.T) {
	done := make(chan struct{})
	close(done)
	if err := waitOrTimeout(done, 5*time.Second); err != nil {
		t.Fatalf("waitOrTimeout(closed done): got err %v, want nil", err)
	}
}

// TestWaitOrTimeout_Timeout asserts the helper returns errDrainTimeout
// when its done channel never closes. This is the core mechanism that
// prevents natsserver.WaitForShutdown from hanging the hub forever (#158).
func TestWaitOrTimeout_Timeout(t *testing.T) {
	never := make(chan struct{})
	start := time.Now()
	err := waitOrTimeout(never, 50*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, errDrainTimeout) {
		t.Fatalf("waitOrTimeout(never): got err %v, want errDrainTimeout", err)
	}
	// Bound the lower edge so we know the timer actually fired (not a
	// fast-path nil), and the upper edge so a regression that swaps in a
	// 30s default timeout fails fast instead of running for half a minute.
	if elapsed < 50*time.Millisecond {
		t.Fatalf("waitOrTimeout: returned in %v, want >= 50ms", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("waitOrTimeout: returned in %v, want < 1s", elapsed)
	}
}

// TestWaitOrTimeout_ZeroFallsBackToDefault asserts that a zero/negative
// timeout uses defaultDrainTimeout rather than firing immediately. The
// happy-path closure still returns nil because the channel closes well
// before the default expires.
func TestWaitOrTimeout_ZeroFallsBackToDefault(t *testing.T) {
	done := make(chan struct{})
	close(done)
	if err := waitOrTimeout(done, 0); err != nil {
		t.Fatalf("waitOrTimeout(zero, closed done): got err %v, want nil", err)
	}
	if err := waitOrTimeout(done, -5*time.Second); err != nil {
		t.Fatalf("waitOrTimeout(negative, closed done): got err %v, want nil", err)
	}
	if defaultDrainTimeout != 30*time.Second {
		t.Errorf("defaultDrainTimeout: got %v, want 30s — brief documents 30s",
			defaultDrainTimeout)
	}
}

// TestRunForeground_PidsWrittenBeforeHubOpen pins the fix for the
// detach pid-race regression introduced by the libfossil-exit migration.
// edgehub.NewHub binds the HTTP listener at construction (the kernel
// SYN-ACKs on the bound socket immediately), so the detach parent's
// waitForTCP would race ahead of the child's writePid if the pid write
// happened post-NewHub. Verifies hub.pid names the foreground process
// by the time openAndSeedHub is invoked. Runs a synthetic seed-failure
// to keep the test fast: we only need to assert hub.pid is present on
// entry into the seed step, not that the full hub bring-up succeeds.
func TestRunForeground_PidsWrittenBeforeHubOpen(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}

	orig := seedHubRepoFunc
	defer func() { seedHubRepoFunc = orig }()
	var hubPidAtSeed int
	seedHubRepoFunc = func(_ context.Context, _ *edgehub.Hub, p paths) error {
		// Read hub.pid at the moment seedHubRepo runs — well after
		// runForeground's writePid call completed but before any of
		// the post-NewHub return paths could clean it up.
		hubPidAtSeed, _ = readPid(p.hubPid)
		return errors.New("synthetic seed failure to short-circuit")
	}

	_ = runForeground(context.Background(), p, opts{})

	if hubPidAtSeed != os.Getpid() {
		t.Errorf("hub.pid at seed time: got %d, want %d (foreground process)",
			hubPidAtSeed, os.Getpid())
	}
}

// TestRunForeground_SeedFailureCleansSidecars asserts that when seed
// fails after libfossil has begun creating SQLite sidecar files,
// runForeground removes the orphan -shm/-wal (and any partial
// hub.fossil) before returning. Without this, a subsequent
// `bones hub start` hits SQLITE_IOERR_SHORT_READ (522) because the
// next CreateWithEnv finds stale WAL/SHM blocks pointing at an absent
// or freshly-recreated parent file. See issue #138.
func TestRunForeground_SeedFailureCleansSidecars(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}

	orig := seedHubRepoFunc
	defer func() { seedHubRepoFunc = orig }()
	seedHubRepoFunc = func(_ context.Context, _ *edgehub.Hub, p paths) error {
		return errors.New("synthetic seed failure")
	}

	err = runForeground(context.Background(), p, opts{})
	if err == nil {
		t.Fatal("expected error from synthetic seed failure, got nil")
	}
	if !strings.Contains(err.Error(), "synthetic seed failure") {
		t.Fatalf("error should wrap synthetic failure, got: %v", err)
	}

	// hub.fossil and its -shm/-wal sidecars must all be cleaned so the
	// next bones hub start does not hit SQLITE_IOERR_SHORT_READ on
	// stale journal state. NewHub creates hub.fossil before
	// seedHubRepoFunc runs; runForeground's seed-failure branch calls
	// h.Stop() (closing the libfossil handle) then removeRepoAndSidecars.
	leftovers := []string{p.hubRepo, p.hubRepo + "-shm", p.hubRepo + "-wal"}
	for _, path := range leftovers {
		if _, statErr := os.Stat(path); statErr == nil {
			t.Errorf("expected %s removed after seed failure; still exists",
				filepath.Base(path))
		} else if !os.IsNotExist(statErr) {
			t.Errorf("stat %s: unexpected error: %v", path, statErr)
		}
	}
}

// TestRunForeground_FreshStartCleansStaleSidecars asserts that stale
// SQLite sidecars from a prior crashed run don't poison the next
// startup with SQLITE_IOERR_SHORT_READ (522). runForeground's
// pre-NewHub cleanup (removeRepoAndSidecars when hub.pid is dead)
// removes them before edgehub.NewHub opens the repo, so NewHub
// succeeds and the synthetic seed-failure path runs cleanly.
// See issue #138.
func TestRunForeground_FreshStartCleansStaleSidecars(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}

	// Plant stale sidecars from a prior failed run. No hub.pid → the
	// fresh-start branch should fire and wipe them before NewHub runs.
	for _, suffix := range []string{"-shm", "-wal"} {
		if err := os.WriteFile(p.hubRepo+suffix, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Synthetic seed failure to keep the rest of runForeground from
	// spinning up real servers. The interesting assertion is that
	// runForeground reaches the seed step at all (it would have failed
	// inside NewHub with SQLITE_IOERR_SHORT_READ if cleanup hadn't run).
	orig := seedHubRepoFunc
	defer func() { seedHubRepoFunc = orig }()
	seedHubRepoFunc = func(_ context.Context, _ *edgehub.Hub, p paths) error {
		return errors.New("synthetic post-cleanup seed failure")
	}

	err = runForeground(context.Background(), p, opts{})
	if err == nil {
		t.Fatal("expected seed-failure error")
	}
	if !strings.Contains(err.Error(), "synthetic post-cleanup seed failure") {
		t.Fatalf("expected synthetic seed-failure error, got: %v", err)
	}
}
