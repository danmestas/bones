package hub

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHubLog_StartingLineFormat asserts the starting line shape. The
// log file accumulates a timestamped INFO line naming pid, repo-port,
// and coord-port — so an operator opening hub.log in a crashed
// workspace can pin the exact ports the hub tried to bind. Uses
// operator vocabulary (hub:, repo-port=, coord-port=) per #247.
func TestHubLog_StartingLineFormat(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns subprocess servers, port-bind sensitive")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Setenv("HOME", t.TempDir())
	root := newGitRepoWithFile(t)
	fossilPort, natsPort := freePort(t), freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	var startErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		startErr = Start(ctx, root,
			WithRepoPort(fossilPort),
			WithCoordPort(natsPort),
		)
	}()
	defer func() {
		cancel()
		wg.Wait()
		if startErr != nil {
			t.Logf("foreground Start exit (post-test): %v", startErr)
		}
	}()

	// Wait until hub.log has the starting + ready lines so we know
	// the foreground process has crossed the post-bind log point.
	waitForHubLogContains(t, root, "hub: ready", 5*time.Second)

	logPath := filepath.Join(root, ".bones", "hub.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub.log: %v", err)
	}
	got := string(data)

	// Pin the starting line: ISO-8601 timestamp + INFO + structured
	// fields. Regex is permissive on the exact timestamp shape but
	// strict on the operator-facing token order.
	re := regexp.MustCompile(
		`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z INFO\s+hub: starting ` +
			`\(pid=\d+, repo-port=` + strconv.Itoa(fossilPort) +
			`, coord-port=` + strconv.Itoa(natsPort) + `\)`,
	)
	if !re.MatchString(got) {
		t.Errorf("hub.log missing well-formed starting line.\nlog:\n%s", got)
	}
	if !strings.Contains(got, "hub: ready") {
		t.Errorf("hub.log missing ready line.\nlog:\n%s", got)
	}
}

// TestHubLog_StoppingThenStopped asserts the ordered shutdown pair:
// `hub: stopping` followed by `hub: stopped` (or a stopped-with-drain-
// error variant). Without ordering, an operator scanning hub.log
// can't tell whether ctx-cancel reached drainHub.
func TestHubLog_StoppingThenStopped(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns subprocess servers")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Setenv("HOME", t.TempDir())
	root := newGitRepoWithFile(t)
	fossilPort, natsPort := freePort(t), freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	var startErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		startErr = Start(ctx, root,
			WithRepoPort(fossilPort),
			WithCoordPort(natsPort),
		)
	}()

	waitForHubLogContains(t, root, "hub: ready", 5*time.Second)
	cancel()
	wg.Wait()
	if startErr != nil {
		t.Logf("foreground Start returned: %v", startErr)
	}

	logPath := filepath.Join(root, ".bones", "hub.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub.log: %v", err)
	}
	got := string(data)

	stoppingIdx := strings.Index(got, "hub: stopping")
	stoppedIdx := strings.Index(got, "hub: stopped")
	if stoppingIdx < 0 {
		t.Fatalf("hub.log missing stopping line.\nlog:\n%s", got)
	}
	if stoppedIdx < 0 {
		t.Fatalf("hub.log missing stopped line.\nlog:\n%s", got)
	}
	if stoppedIdx <= stoppingIdx {
		t.Fatalf("hub: stopped should come after hub: stopping.\nlog:\n%s", got)
	}
}

// TestHubLog_AppendAcrossCycles asserts hub.log accumulates across
// repeated start/stop cycles instead of truncating each run. An
// operator auditing a workspace that has crashed-and-restarted needs
// the full timeline.
func TestHubLog_AppendAcrossCycles(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns subprocess servers")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Setenv("HOME", t.TempDir())
	root := newGitRepoWithFile(t)
	fossilPort, natsPort := freePort(t), freePort(t)

	for cycle := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		var startErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			startErr = Start(ctx, root,
				WithRepoPort(fossilPort),
				WithCoordPort(natsPort),
			)
		}()
		waitForHubLogContains(t, root, "hub: ready", 5*time.Second)
		cancel()
		wg.Wait()
		if startErr != nil {
			t.Logf("cycle %d Start returned: %v", cycle, startErr)
		}
	}

	logPath := filepath.Join(root, ".bones", "hub.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub.log: %v", err)
	}
	got := string(data)

	// Two cycles → at least two starting lines, two ready lines, two
	// stopping lines, two stopped lines.
	if n := strings.Count(got, "hub: starting"); n < 2 {
		t.Errorf("expected >=2 starting lines across cycles, got %d.\nlog:\n%s", n, got)
	}
	if n := strings.Count(got, "hub: ready"); n < 2 {
		t.Errorf("expected >=2 ready lines across cycles, got %d.\nlog:\n%s", n, got)
	}
	if n := strings.Count(got, "hub: stopping"); n < 2 {
		t.Errorf("expected >=2 stopping lines across cycles, got %d.\nlog:\n%s", n, got)
	}
}

// TestHubLog_NoSubstrateVocabulary asserts lifecycle events use bones
// vocabulary only — no `fossil`, `nats`, or `jetstream`. Substrate
// names leak abstraction across the bones boundary; operators reading
// hub.log should see hub:/repo:/coord: only (#247).
func TestHubLog_NoSubstrateVocabulary(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns subprocess servers")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Setenv("HOME", t.TempDir())
	root := newGitRepoWithFile(t)
	fossilPort, natsPort := freePort(t), freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	var startErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		startErr = Start(ctx, root,
			WithRepoPort(fossilPort),
			WithCoordPort(natsPort),
		)
	}()

	waitForHubLogContains(t, root, "hub: ready", 5*time.Second)
	cancel()
	wg.Wait()
	if startErr != nil {
		t.Logf("foreground Start returned: %v", startErr)
	}

	logPath := filepath.Join(root, ".bones", "hub.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hub.log: %v", err)
	}
	got := strings.ToLower(string(data))

	// Whitelist the lifecycle line prefixes we add; only THOSE lines
	// must be substrate-free. Substrate words elsewhere in hub.log
	// (e.g., legacy "hub: fossil at ..." stdout banner that lands here
	// in the detached child path) are out of scope for this issue.
	for _, line := range strings.Split(got, "\n") {
		// Match lines emitted by hubLogger only — they have the
		// "<ts> <LEVEL> hub:" shape with the level field padded.
		if !strings.Contains(line, " hub: starting") &&
			!strings.Contains(line, " hub: ready") &&
			!strings.Contains(line, " hub: stopping") &&
			!strings.Contains(line, " hub: stopped") &&
			!strings.Contains(line, " hub: child exited") {
			continue
		}
		for _, banned := range []string{"fossil", "nats", "jetstream"} {
			if strings.Contains(line, banned) {
				t.Errorf("lifecycle line contains substrate word %q: %q", banned, line)
			}
		}
	}
}

// waitForHubLogContains polls .bones/hub.log until it contains needle,
// or t.Fatalfs on timeout. Used to synchronize with async log writes
// from the foreground hub goroutine.
func waitForHubLogContains(t *testing.T, root, needle string, timeout time.Duration) {
	t.Helper()
	logPath := filepath.Join(root, ".bones", "hub.log")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(data), needle) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("hub.log never contained %q within %s", needle, timeout)
}
