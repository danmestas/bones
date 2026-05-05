package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"
)

// TestApplySlot_MaterializesLatestRecovery verifies that
// `bones apply --slot=<name> --to=<dir>` copies the latest recovery
// dir's contents into <dir>, preserving the relative path layout but
// hiding the timestamped scheme from the operator.
func TestApplySlot_MaterializesLatestRecovery(t *testing.T) {
	dir := buildSlotRecoveryFixture(t, "alpha", map[string]string{
		"research/alpha/findings.md": "alpha findings\n",
		"research/alpha/SUMMARY.md":  "alpha summary\n",
	}, time.Now())

	target := filepath.Join(dir, "out")
	cmd := &ApplyCmd{Slot: "alpha", To: target}
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for rel, want := range map[string]string{
		"research/alpha/findings.md": "alpha findings\n",
		"research/alpha/SUMMARY.md":  "alpha summary\n",
	} {
		got, err := os.ReadFile(filepath.Join(target, rel))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// TestApplySlot_NoRecoveryDir verifies the documented error when no
// recovery dir exists for the slot.
func TestApplySlot_NoRecoveryDir(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, ".bones"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ".bones", "agent.id"),
		[]byte("slot-test-agent\n"), 0o644))

	t.Chdir(dir)
	cmd := &ApplyCmd{Slot: "ghost", To: filepath.Join(dir, "out")}
	err := cmd.Run(&libfossilcli.Globals{})
	if err == nil ||
		!strings.Contains(err.Error(), "slot ghost has no committed artifacts") {
		t.Fatalf("expected 'no committed artifacts' error, got %v", err)
	}
}

// TestApplySlot_PicksMostRecentByMtime verifies the multi-recovery
// tiebreaker: when several recovery dirs exist for the same slot
// (multiple sessions), pick the most-recent-by-mtime.
func TestApplySlot_PicksMostRecentByMtime(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, ".bones"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ".bones", "agent.id"),
		[]byte("slot-test-agent\n"), 0o644))

	older := filepath.Join(dir, ".bones", "recovery", "beta-100")
	newer := filepath.Join(dir, ".bones", "recovery", "beta-200")
	must(t, os.MkdirAll(older, 0o755))
	must(t, os.MkdirAll(newer, 0o755))
	must(t, os.WriteFile(filepath.Join(older, "marker.txt"),
		[]byte("OLD\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(newer, "marker.txt"),
		[]byte("NEW\n"), 0o644))
	// Force older's mtime to be in the past so the tie-breaker is
	// unambiguous (mkdir order is not enough on filesystems with
	// coarse mtime resolution).
	past := now.Add(-2 * time.Hour)
	must(t, os.Chtimes(older, past, past))
	future := now.Add(1 * time.Hour)
	must(t, os.Chtimes(newer, future, future))

	t.Chdir(dir)
	target := filepath.Join(dir, "out")
	cmd := &ApplyCmd{Slot: "beta", To: target}
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "marker.txt"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if strings.TrimSpace(string(got)) != "NEW" {
		t.Errorf("marker = %q, want NEW (newer recovery dir)", got)
	}
}

// TestApplySlot_WithoutToFlagErrors verifies that --slot without --to
// is a usage error.
func TestApplySlot_WithoutToFlagErrors(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, ".bones"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ".bones", "agent.id"),
		[]byte("slot-test-agent\n"), 0o644))

	t.Chdir(dir)
	cmd := &ApplyCmd{Slot: "alpha"}
	err := cmd.Run(&libfossilcli.Globals{})
	if err == nil || !strings.Contains(err.Error(), "--to=<dir>") {
		t.Fatalf("expected '--to=<dir>' usage error, got %v", err)
	}
}

// TestApplySlot_OverwritesExistingFiles verifies that pre-existing
// files in the target dir get overwritten silently — operator's
// responsibility, per spec.
func TestApplySlot_OverwritesExistingFiles(t *testing.T) {
	dir := buildSlotRecoveryFixture(t, "gamma", map[string]string{
		"a.txt": "from-recovery\n",
	}, time.Now())

	target := filepath.Join(dir, "out")
	must(t, os.MkdirAll(target, 0o755))
	must(t, os.WriteFile(filepath.Join(target, "a.txt"),
		[]byte("stale-content\n"), 0o644))

	cmd := &ApplyCmd{Slot: "gamma", To: target}
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-recovery\n" {
		t.Errorf("a.txt = %q, want overwritten with recovery content", got)
	}
}

// TestApplySlot_CreatesTargetDir verifies the target dir is created
// if it doesn't already exist.
func TestApplySlot_CreatesTargetDir(t *testing.T) {
	dir := buildSlotRecoveryFixture(t, "delta", map[string]string{
		"x.md": "x\n",
	}, time.Now())

	target := filepath.Join(dir, "nested", "deeper", "out")
	cmd := &ApplyCmd{Slot: "delta", To: target}
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "x.md")); err != nil {
		t.Errorf("target dir not created or file missing: %v", err)
	}
}

// buildSlotRecoveryFixture creates a minimal bones workspace with one
// recovery dir for `slot` populated with `files` (rel-path → content).
// `now` controls the recovery dir's timestamp suffix and is also used
// to force consistent mtimes for tiebreaker tests. Chdirs to the
// workspace dir.
func buildSlotRecoveryFixture(
	t *testing.T, slot string, files map[string]string, now time.Time,
) string {
	t.Helper()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, ".bones"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ".bones", "agent.id"),
		[]byte("slot-test-agent\n"), 0o644))

	recoveryName := slot + "-" + itoa(now.Unix())
	recoveryDir := filepath.Join(dir, ".bones", "recovery", recoveryName)
	must(t, os.MkdirAll(recoveryDir, 0o755))
	for rel, content := range files {
		full := filepath.Join(recoveryDir, rel)
		must(t, os.MkdirAll(filepath.Dir(full), 0o755))
		must(t, os.WriteFile(full, []byte(content), 0o644))
	}
	t.Chdir(dir)
	return dir
}

// itoa is a tiny stdlib-free int64→string helper to keep the fixture
// helper free of strconv churn.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
