package hub

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestStop_EscalatesToSIGKILLOnTermResistant pins the contract that
// hub.Stop kills processes that ignore SIGTERM. The pre-#138 Stop
// signaled SIGTERM once, removed the pid file immediately, and
// returned — leaving SIGTERM-resistant processes as orphans. The
// fixed Stop waits, escalates to SIGKILL, and only removes the pid
// file once the process is confirmed dead.
//
// Skipped on Windows: relies on POSIX `trap`/`SIGKILL` semantics.
func TestStop_EscalatesToSIGKILLOnTermResistant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only: Windows lacks SIGTERM/SIGKILL semantics")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}

	// Spawn a process that ignores SIGTERM but yields to SIGKILL.
	// Python's signal.SIG_IGN is rock-solid; the script also prints
	// "ready" on stdout so we can synchronize on the handler being
	// installed before Stop runs. Without this handshake there is a
	// race where Stop's SIGTERM lands before python's signal.signal
	// call, killing the subprocess on the default handler. Skip if
	// python3 is unavailable.
	pyPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; required for TERM-ignore subprocess")
	}
	cmd := exec.Command(pyPath, "-c",
		"import signal,sys,time\n"+
			"signal.signal(signal.SIGTERM, signal.SIG_IGN)\n"+
			"sys.stdout.write('ready\\n')\n"+
			"sys.stdout.flush()\n"+
			"time.sleep(60)\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn term-resistant process: %v", err)
	}
	pid := cmd.Process.Pid

	// Wait for python to confirm the handler is installed. Bound the
	// wait so a broken interpreter doesn't make this test hang.
	readyCh := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			readyCh <- err
			return
		}
		if line != "ready\n" {
			readyCh <- nil
			return
		}
		readyCh <- nil
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			t.Fatalf("read ready: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("python subprocess never reported ready")
	}

	// Reap asynchronously via cmd.Wait. Without an active Wait, the
	// child becomes a zombie after kill — and Signal(0) on a zombie
	// returns success on macOS/Linux, which would make pid-liveness
	// probes lie about death. Closing `done` (vs. sending) lets both
	// the test body and a fallback path read without contention.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	if err := writePid(p.hubPid, pid); err != nil {
		t.Fatalf("writePid: %v", err)
	}

	stopStart := time.Now()
	if err := Stop(root); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopElapsed := time.Since(stopStart)
	t.Logf("Stop returned after %v (expect ~stopGrace=%v due to ignored TERM)",
		stopElapsed, stopGrace)
	if stopElapsed < stopGrace/2 {
		t.Fatalf("Stop returned too quickly (%v); SIGTERM appears to "+
			"have killed the trap-resistant child, meaning the test "+
			"is not actually exercising SIGKILL escalation", stopElapsed)
	}

	// After Stop returns, the child must exit. Bound the wait so a
	// missing SIGKILL escalation surfaces as a failure, not a hang.
	// On the timeout path, kill manually so the cmd.Wait goroutine
	// (and the test process) can shut down cleanly.
	select {
	case <-done:
		return
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("pid %d still alive after Stop returned; SIGTERM "+
			"was ignored and SIGKILL escalation is missing (#138)", pid)
	}
}
