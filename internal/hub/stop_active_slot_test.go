package hub

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStop_RefusesWithActiveSlots asserts the active-slot safety guard
// in #157. Stop without WithForce must return ErrActiveSlots when any
// .bones/swarm/<slot>/leaf.pid points at a live process, naming the
// offending slot(s) and the --force escape hatch in the error string.
func TestStop_RefusesWithActiveSlots(t *testing.T) {
	root := t.TempDir()

	// Plant a live "leaf" — sleep is portable and harmless.
	pidPath, sleepCmd := plantLiveSlot(t, root, "test-slot")
	defer cleanupCmd(sleepCmd)

	err := Stop(root)
	if !errors.Is(err, ErrActiveSlots) {
		t.Fatalf("Stop with active slot: got err %v, want ErrActiveSlots", err)
	}
	if !strings.Contains(err.Error(), "test-slot") {
		t.Errorf("error should name the active slot; got: %v", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should mention --force escape hatch; got: %v", err)
	}

	// Pid file must still exist — Stop refused, no destructive side-effect.
	if _, statErr := os.Stat(pidPath); statErr != nil {
		t.Errorf("leaf.pid should be preserved when Stop refuses: %v", statErr)
	}
}

// TestStop_WithForceOverrides asserts that Stop(root, WithForce(true))
// proceeds despite active slots. No leaf process is killed by Stop —
// the guard is a CLI-level safety, not a leaf-reaper. Stop just stops
// declining to teardown the hub processes.
func TestStop_WithForceOverrides(t *testing.T) {
	root := t.TempDir()
	_, sleepCmd := plantLiveSlot(t, root, "force-slot")
	defer cleanupCmd(sleepCmd)

	// No hub pid files exist, so Stop's signal loop is a no-op. The
	// only thing this asserts is that the active-slot check does not
	// reject the call when WithForce(true) is passed.
	if err := Stop(root, WithForce(true)); err != nil {
		t.Fatalf("Stop --force with active slot: got err %v, want nil", err)
	}
}

// TestStop_PreservesURLFiles asserts the port-preservation contract
// (#157): Stop must NOT remove .bones/hub-{fossil,nats}-url so the
// next Start re-reads the recorded port via resolvePorts. When the
// port is still free, the new hub binds the same port and active
// leaves' cached NATS URLs keep working across the restart.
//
// bones down is the channel for full URL clearing — it removes the
// entire .bones/ directory (planRemoveBonesDir).
func TestStop_PreservesURLFiles(t *testing.T) {
	root := t.TempDir()
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.orchDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Plant URL files as if a Start had bound real ports.
	if err := os.WriteFile(p.fossilURL, []byte("http://127.0.0.1:60778\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.natsURL, []byte("nats://127.0.0.1:60779\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Stop(root); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Both URL files must survive so resolvePorts reuses the recorded
	// port on next Start.
	for _, urlFile := range []string{p.fossilURL, p.natsURL} {
		if _, statErr := os.Stat(urlFile); statErr != nil {
			t.Errorf("%s should be preserved across Stop: %v",
				filepath.Base(urlFile), statErr)
		}
	}
}

// TestActiveSlotNames_NoSwarmDir asserts activeSlotNames returns
// (nil, nil) when .bones/swarm doesn't exist, so Stop on a fresh
// workspace doesn't trip on a missing directory.
func TestActiveSlotNames_NoSwarmDir(t *testing.T) {
	root := t.TempDir()
	names, err := activeSlotNames(root)
	if err != nil {
		t.Fatalf("activeSlotNames(empty workspace): err %v", err)
	}
	if len(names) != 0 {
		t.Errorf("activeSlotNames(empty): got %v, want empty", names)
	}
}

// plantLiveSlot creates .bones/swarm/<slot>/leaf.pid pointing at a
// live `sleep 30` subprocess. Returns the pid file path and the
// command so the test can clean up. The sleep duration is long enough
// that it stays alive for any reasonable test runtime, short enough
// that an aborted test doesn't leave a long-running orphan.
func plantLiveSlot(t *testing.T, root, slot string) (string, *exec.Cmd) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("plantLiveSlot relies on `sleep`; skipping on windows")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}

	slotDir := filepath.Join(root, ".bones", "swarm", slot)
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("mkdir slot dir: %v", err)
	}
	pidPath := filepath.Join(slotDir, "leaf.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("write leaf.pid: %v", err)
	}

	// Brief settle so the process is observable in /proc / kill -0.
	time.Sleep(20 * time.Millisecond)
	return pidPath, cmd
}

func cleanupCmd(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
