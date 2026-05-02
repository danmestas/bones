package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPrintHubStatus_FreshScaffold pins shape A: a fresh `bones up`
// has no hub-fossil-url or hub-nats-url file yet. printHubStatus
// must announce the lazy-start expectation, not pretend silence.
func TestPrintHubStatus_FreshScaffold(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	printHubStatus(&buf, root)

	got := buf.String()
	if !strings.Contains(got, "not yet started") {
		t.Errorf("fresh scaffold should announce lazy start; got: %q", got)
	}
	if !strings.Contains(got, "SessionStart") {
		t.Errorf("fresh scaffold should name the lifecycle hook; got: %q", got)
	}
}

// TestPrintHubStatus_HubRunning pins shape B: URL files exist and
// both pids point at live processes (we use os.Getpid() as the
// canonical live pid). printHubStatus must echo the URLs and pid so
// the operator has a concrete handle.
func TestPrintHubStatus_HubRunning(t *testing.T) {
	root := t.TempDir()
	bones := filepath.Join(root, ".bones")
	if err := os.MkdirAll(filepath.Join(bones, "pids"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "hub-fossil-url"),
		[]byte("http://127.0.0.1:51234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "hub-nats-url"),
		[]byte("nats://127.0.0.1:51235\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := strconv.Itoa(os.Getpid())
	for _, name := range []string{"fossil.pid", "nats.pid"} {
		if err := os.WriteFile(filepath.Join(bones, "pids", name),
			[]byte(live+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	printHubStatus(&buf, root)

	got := buf.String()
	if !strings.Contains(got, "http://127.0.0.1:51234") {
		t.Errorf("running shape must echo fossil URL; got: %q", got)
	}
	if !strings.Contains(got, "nats://127.0.0.1:51235") {
		t.Errorf("running shape must echo nats URL; got: %q", got)
	}
	if !strings.Contains(got, "pid="+live) {
		t.Errorf("running shape must echo pid; got: %q", got)
	}
	if strings.Contains(got, "not yet started") {
		t.Errorf("running shape must NOT announce lazy start; got: %q", got)
	}
}

// TestPrintHubStatus_StaleURLs pins shape C: URL files exist but
// the recorded pid is dead (the hub was once running, has since
// stopped, but URLs remain). printHubStatus must signal "stale" so
// the operator doesn't think the URLs are usable right now.
func TestPrintHubStatus_StaleURLs(t *testing.T) {
	root := t.TempDir()
	bones := filepath.Join(root, ".bones")
	if err := os.MkdirAll(filepath.Join(bones, "pids"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "hub-fossil-url"),
		[]byte("http://127.0.0.1:51234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "hub-nats-url"),
		[]byte("nats://127.0.0.1:51235\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 999999 is a high pid that will not be in use on a normal host.
	for _, name := range []string{"fossil.pid", "nats.pid"} {
		if err := os.WriteFile(filepath.Join(bones, "pids", name),
			[]byte("999999\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	printHubStatus(&buf, root)

	got := buf.String()
	if !strings.Contains(got, "previously recorded") {
		t.Errorf("stale URL shape must say 'previously recorded'; got: %q", got)
	}
	if !strings.Contains(got, "restart on next verb") {
		t.Errorf("stale URL shape must explain next-verb restart; got: %q", got)
	}
	if strings.Contains(got, "(pid=") {
		t.Errorf("stale URL shape must NOT echo a pid (the recorded one "+
			"is dead); got: %q", got)
	}
}
