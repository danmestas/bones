package registry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestListInfoEnumerates seeds two registry files (each backed by a real
// workspace directory with .bones/agent.id) and asserts that ListInfo
// returns both, with ID, AgentID, LastTouched, and HubStatus populated.
//
// HubStatus is asserted as HubStopped: the entries reference fake PIDs
// and unreachable URLs so IsAlive returns false. We don't assert
// HubRunning here — that requires a live hub and is exercised by
// `bones status --all` integration tests.
func TestListInfoEnumerates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	wsA := t.TempDir()
	wsB := t.TempDir()
	mustWriteAgentID(t, wsA, "agent-a")
	mustWriteAgentID(t, wsB, "agent-b")

	now := time.Now().UTC().Truncate(time.Second)
	// Use the test's own pid for HubPID — alive on this host, so the
	// read-time prune (#229) keeps the entries. The probeStatus call
	// still resolves to HubStopped because the URL is unreachable.
	pid := os.Getpid()
	entries := []Entry{
		{
			Cwd: wsA, Name: "alpha",
			HubURL:    "http://127.0.0.1:1", // unreachable
			NATSURL:   "nats://127.0.0.1:1",
			HubPID:    pid,
			StartedAt: now,
		},
		{
			Cwd: wsB, Name: "beta",
			HubURL:    "http://127.0.0.1:2",
			NATSURL:   "nats://127.0.0.1:2",
			HubPID:    pid,
			StartedAt: now,
		},
	}
	for _, e := range entries {
		if err := Write(e); err != nil {
			t.Fatalf("Write(%s): %v", e.Name, err)
		}
	}

	got, err := ListInfo()
	if err != nil {
		t.Fatalf("ListInfo: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListInfo len = %d, want 2 (got %+v)", len(got), got)
	}

	// Sorted by Name: alpha < beta.
	if got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Fatalf("sort order: got %q,%q want alpha,beta",
			got[0].Name, got[1].Name)
	}

	for _, info := range got {
		if info.ID == "" {
			t.Errorf("%s: ID empty", info.Name)
		}
		if info.ID != WorkspaceID(info.Cwd) {
			t.Errorf("%s: ID = %q, want %q",
				info.Name, info.ID, WorkspaceID(info.Cwd))
		}
		if info.LastTouched.IsZero() {
			t.Errorf("%s: LastTouched zero", info.Name)
		}
		if info.HubStatus != HubStopped {
			t.Errorf("%s: HubStatus = %q, want %q",
				info.Name, info.HubStatus, HubStopped)
		}
	}

	if got[0].AgentID != "agent-a" {
		t.Errorf("alpha agent_id = %q, want agent-a", got[0].AgentID)
	}
	if got[1].AgentID != "agent-b" {
		t.Errorf("beta agent_id = %q, want agent-b", got[1].AgentID)
	}
}

// TestListInfoEmpty: no registry directory present.
func TestListInfoEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := ListInfo()
	if err != nil {
		t.Fatalf("ListInfo on empty home: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}

// TestListInfoSkipsCorrupt: a malformed JSON file in the registry dir
// must not abort the listing — corrupt files are silently skipped.
func TestListInfoSkipsCorrupt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	good := t.TempDir()
	mustWriteAgentID(t, good, "good")
	if err := Write(Entry{
		Cwd: good, Name: "good",
		HubURL: "http://127.0.0.1:0", HubPID: os.Getpid(),
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	bogus := filepath.Join(RegistryDir(), "deadbeefdeadbeef.json")
	if err := os.WriteFile(bogus, []byte("not json"), 0o644); err != nil {
		t.Fatalf("write bogus: %v", err)
	}
	got, err := ListInfo()
	if err != nil {
		t.Fatalf("ListInfo: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("expected just [good], got %+v", got)
	}
}

// TestListInfoMissingAgentID: registry entry whose cwd exists but
// has no .bones/agent.id (workspace marker scrubbed underneath us)
// reports AgentID="". Pre-#229 this test used a non-existent cwd
// path; #229's read-time self-prune now removes such entries (cwd
// gone == registry crud), so the test uses an existing-but-marker-
// less directory to preserve the original intent.
func TestListInfoMissingAgentID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bare := t.TempDir() // exists, no .bones/agent.id marker
	if err := Write(Entry{
		Cwd: bare, Name: "bare",
		HubURL: "http://127.0.0.1:0", HubPID: os.Getpid(),
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := ListInfo()
	if err != nil {
		t.Fatalf("ListInfo: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].AgentID != "" {
		t.Errorf("AgentID = %q, want empty", got[0].AgentID)
	}
	if got[0].HubStatus != HubStopped {
		t.Errorf("HubStatus = %q, want stopped", got[0].HubStatus)
	}
}

// TestProbeStatusUnknown: entries lacking HubURL or HubPID = HubUnknown.
func TestProbeStatusUnknown(t *testing.T) {
	if got := probeStatus(Entry{}); got != HubUnknown {
		t.Errorf("empty entry: got %q, want %q", got, HubUnknown)
	}
	if got := probeStatus(Entry{HubURL: "http://x"}); got != HubUnknown {
		t.Errorf("missing pid: got %q, want %q", got, HubUnknown)
	}
	if got := probeStatus(Entry{HubPID: 1}); got != HubUnknown {
		t.Errorf("missing url: got %q, want %q", got, HubUnknown)
	}
}

func mustWriteAgentID(t *testing.T, root, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, ".bones", "agent.id"),
		[]byte(id+"\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
}
