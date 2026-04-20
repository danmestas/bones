package workspace

import (
	"os"
	"testing"
)

func TestPackageBuilds(t *testing.T) {
	// Sanity: exported symbols compile and sentinel errors are distinct.
	errs := []error{ErrAlreadyInitialized, ErrNoWorkspace, ErrLeafUnreachable, ErrLeafStartTimeout}
	seen := map[error]bool{}
	for _, e := range errs {
		if e == nil {
			t.Fatal("nil sentinel")
		}
		if seen[e] {
			t.Fatalf("duplicate sentinel: %v", e)
		}
		seen[e] = true
	}
}

func TestConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	orig := config{
		Version:     configVersion,
		AgentID:     "agent-123",
		NATSURL:     "nats://127.0.0.1:4222",
		LeafHTTPURL: "http://127.0.0.1:51234",
		RepoPath:    "repo.fossil",
		CreatedAt:   "2026-04-20T14:45:00Z",
	}
	path := dir + "/config.json"
	if err := saveConfig(path, orig); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	got, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got != orig {
		t.Fatalf("round-trip mismatch:\n got:  %+v\n want: %+v", got, orig)
	}
}

func TestConfig_RejectsUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("expected error for unknown version, got nil")
	}
}
