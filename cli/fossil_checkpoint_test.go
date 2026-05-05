package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/modernc"
)

// TestPassiveCheckpoint_MakesHubVanillaFossilReadable is the regression
// test for bones #211 / #212. It builds a bones-style hub.fossil that
// libfossil has just written (so the WAL is un-checkpointed and the
// main file is the 4 KiB stub vanilla fossil chokes on), confirms
// vanilla fossil DOES reject it, runs passiveCheckpointHubFossil,
// then confirms vanilla fossil now accepts it.
//
// Without the fix this test fails at the post-checkpoint assertion:
// the main file stays at 4 KiB and `fossil info` still prints
// `not a valid repository`.
func TestPassiveCheckpoint_MakesHubVanillaFossilReadable(t *testing.T) {
	fossilBin, err := exec.LookPath("fossil")
	if err != nil {
		t.Skip("vanilla fossil binary not on PATH; regression test requires it")
	}

	dir := t.TempDir()
	hubPath := filepath.Join(dir, "hub.fossil")

	// Build the fixture exactly the way bones builds hub.fossil in real
	// workspaces: libfossil.Create + an initial commit. Keep the repo
	// handle open for the rest of the test so the scenario mirrors a
	// running `bones hub` (file open, WAL un-checkpointed, hub still
	// holding write locks). PASSIVE checkpoint must work in this state.
	repo, err := libfossil.Create(hubPath, libfossil.CreateOpts{User: "hub"})
	if err != nil {
		t.Fatalf("libfossil.Create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if _, _, err := repo.Commit(libfossil.CommitOpts{
		Files:   []libfossil.FileToCommit{{Name: "seed.txt", Content: []byte("seed\n")}},
		Comment: "init",
		User:    "hub",
	}); err != nil {
		t.Fatalf("repo.Commit: %v", err)
	}

	// Sanity check: the bug surface must actually be present pre-fix.
	// hub.fossil should be a 4 KiB stub with bulk content sitting in
	// the WAL sidecar. If this assertion ever stops holding, libfossil
	// or modernc/sqlite changed their default journaling and the
	// regression test no longer exercises the original bug — fail
	// loudly so a human looks.
	mainStat, err := os.Stat(hubPath)
	if err != nil {
		t.Fatalf("stat hub.fossil: %v", err)
	}
	if mainStat.Size() > 16*1024 {
		t.Fatalf("expected hub.fossil to be a small WAL stub (<=16KiB), "+
			"got %d bytes — fixture no longer reproduces #211", mainStat.Size())
	}
	walStat, err := os.Stat(hubPath + "-wal")
	if err != nil || walStat.Size() == 0 {
		t.Fatalf("expected non-empty hub.fossil-wal sidecar, got err=%v size=%d", err, walStat)
	}

	// Confirm the bug: vanilla fossil rejects the un-checkpointed file.
	out, _ := exec.Command(fossilBin, "info", "-R", hubPath).CombinedOutput()
	if !containsNotValid(string(out)) {
		t.Fatalf("expected vanilla fossil to reject hub.fossil before "+
			"checkpoint, got: %q", string(out))
	}

	// Apply the fix.
	passiveCheckpointHubFossil(hubPath)

	// Vanilla fossil must now accept the file.
	out, err = exec.Command(fossilBin, "info", "-R", hubPath).CombinedOutput()
	if err != nil {
		t.Fatalf("after PASSIVE checkpoint, vanilla fossil still failed: "+
			"err=%v output=%q", err, string(out))
	}
	if containsNotValid(string(out)) {
		t.Fatalf("after PASSIVE checkpoint, vanilla fossil still says "+
			"`not a valid repository`: %q", string(out))
	}
}

// TestPassiveCheckpoint_MissingFile_FailsSoft documents the contract
// that a missing/unreadable hub.fossil must NOT crash the caller.
// Callers (peek, status, swarm fan-in, apply) treat checkpoint as
// best-effort and proceed to the vanilla-fossil shell-out regardless;
// that contract is what makes it safe to drop this helper into every
// shell-out path.
func TestPassiveCheckpoint_MissingFile_FailsSoft(t *testing.T) {
	// Should not panic and should not block — just emits a warning.
	passiveCheckpointHubFossil(filepath.Join(t.TempDir(), "does-not-exist.fossil"))
}

// containsNotValid checks for the vanilla-fossil rejection signature.
// Kept narrow so a future fossil version that reworks the message
// surfaces as a test failure (and a prompt to revisit this fix).
func containsNotValid(s string) bool {
	return len(s) > 0 && (indexOf(s, "not a valid repository") >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
