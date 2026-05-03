package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiagnoseSwarmFailure_NoWorkspace asserts the helper degrades
// gracefully when called before workspace.Join has captured a
// workspace dir — used by callers that surface the diagnostic on
// any swarm verb error, including those that fail before info is
// populated.
func TestDiagnoseSwarmFailure_NoWorkspace(t *testing.T) {
	got := diagnoseSwarmFailure("", "nats://127.0.0.1:0")
	if !strings.Contains(got, "no workspace context") {
		t.Errorf("empty workspace dir: got %q, want a 'no workspace context' line", got)
	}
}

// TestDiagnoseSwarmFailure_HubDown covers the most actionable case:
// a swarm verb fails because the hub is gone. The diagnostic must
// say so explicitly so the operator does not waste time on URL
// races.
func TestDiagnoseSwarmFailure_HubDown(t *testing.T) {
	root := t.TempDir()
	got := diagnoseSwarmFailure(root, "nats://127.0.0.1:55555")
	if !strings.Contains(got, "DOWN") {
		t.Errorf("hub down: got %q, want a 'DOWN' marker", got)
	}
	if !strings.Contains(got, "nats://127.0.0.1:55555") {
		t.Errorf("got %q, want connected URL surfaced", got)
	}
}

// TestDiagnoseSwarmFailure_URLMismatch is the smoking-gun case the
// instrumentation was added for (#155): the connected URL diverges
// from the URL recorded on disk. The diagnostic must call this out
// explicitly so the operator pivots straight to the URL discovery
// path rather than chasing JetStream readiness.
func TestDiagnoseSwarmFailure_URLMismatch(t *testing.T) {
	root := t.TempDir()
	pidsDir := filepath.Join(root, ".bones", "pids")
	if err := os.MkdirAll(pidsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, ".bones", "hub-nats-url"),
		[]byte("nats://127.0.0.1:60000\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	got := diagnoseSwarmFailure(root, "nats://127.0.0.1:55555")
	if !strings.Contains(got, "MISMATCH") {
		t.Errorf("URL mismatch: got %q, want 'MISMATCH' marker", got)
	}
	if !strings.Contains(got, "nats://127.0.0.1:55555") {
		t.Errorf("got %q, want connected URL %q", got, "nats://127.0.0.1:55555")
	}
	if !strings.Contains(got, "nats://127.0.0.1:60000") {
		t.Errorf("got %q, want disk URL %q", got, "nats://127.0.0.1:60000")
	}
}

// TestDiagnoseSwarmFailure_URLAgreesNoMismatch confirms the no-false-
// alarm case: when the connected URL matches the disk URL, the
// diagnostic must NOT print a MISMATCH marker. False positives here
// would steer operators toward URL races for failures that are
// actually JetStream-readiness or KV-bucket issues.
func TestDiagnoseSwarmFailure_URLAgreesNoMismatch(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, ".bones", "hub-nats-url"),
		[]byte("nats://127.0.0.1:55555\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	got := diagnoseSwarmFailure(root, "nats://127.0.0.1:55555")
	if strings.Contains(got, "MISMATCH") {
		t.Errorf("matching URLs should not flag MISMATCH; got %q", got)
	}
}
