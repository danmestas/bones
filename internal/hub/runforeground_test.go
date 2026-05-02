package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunForeground_SeedFailureCleansSidecars asserts that when seed
// fails after libfossil has begun creating SQLite sidecar files,
// runForeground removes the orphan -shm/-wal (and any partial
// hub.fossil) before returning. Without this, a subsequent
// `bones hub start` hits SQLITE_IOERR_SHORT_READ (522) because the
// next CreateWithEnv finds stale WAL/SHM blocks pointing at an absent
// or freshly-recreated parent file. See issue #138.
func TestRunForeground_SeedFailureCleansSidecars(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones", "pids"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}

	orig := seedHubRepoFunc
	defer func() { seedHubRepoFunc = orig }()
	seedHubRepoFunc = func(p paths) error {
		// Simulate libfossil partial init: a 0-byte hub.fossil and the
		// SQLite -shm / -wal sidecars land before schema apply errors.
		if err := os.WriteFile(p.hubRepo, []byte{}, 0o644); err != nil {
			return err
		}
		for _, suffix := range []string{"-shm", "-wal"} {
			if err := os.WriteFile(p.hubRepo+suffix, []byte{}, 0o644); err != nil {
				return err
			}
		}
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
	// stale journal state.
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

// TestRunForeground_FreshStartCleansStaleSidecars asserts the
// pre-seed cleanup at the top of runForeground also removes
// SQLite sidecars left from a prior crash. Otherwise the very next
// seedHubRepo call inherits stale WAL state and reports a short-read.
// See issue #138.
func TestRunForeground_FreshStartCleansStaleSidecars(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones", "pids"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}

	// Plant stale sidecars from a prior failed run. No fossil.pid → the
	// fresh-start branch should fire and wipe them.
	for _, suffix := range []string{"-shm", "-wal"} {
		if err := os.WriteFile(p.hubRepo+suffix, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Replace seed with one that observes whether the sidecars are
	// gone by the time it's invoked. Returns an error to keep the rest
	// of runForeground from spinning up real servers.
	orig := seedHubRepoFunc
	defer func() { seedHubRepoFunc = orig }()
	var sawSidecars bool
	seedHubRepoFunc = func(p paths) error {
		for _, suffix := range []string{"-shm", "-wal"} {
			if _, err := os.Stat(p.hubRepo + suffix); err == nil {
				sawSidecars = true
			}
		}
		return errors.New("synthetic post-cleanup seed failure")
	}

	_ = runForeground(context.Background(), p, opts{})
	if sawSidecars {
		t.Fatal("fresh-start should remove stale -shm/-wal before seedHubRepo " +
			"runs; sidecars still present")
	}
}
