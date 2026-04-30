package telemetry

import "testing"

func TestWorkspaceHash_Stable(t *testing.T) {
	a := WorkspaceHash("/Users/alice/projects/foo")
	b := WorkspaceHash("/Users/alice/projects/foo")
	if a != b {
		t.Fatalf("same path produced different hashes: %q vs %q", a, b)
	}
	if len(a) != 12 {
		t.Errorf("expected 12-char hash, got %d chars: %q", len(a), a)
	}
}

func TestWorkspaceHash_DifferentPathsDifferentHashes(t *testing.T) {
	a := WorkspaceHash("/Users/alice/projects/foo")
	b := WorkspaceHash("/Users/alice/projects/bar")
	if a == b {
		t.Fatalf("different paths produced same hash: %q", a)
	}
}

func TestWorkspaceHash_NormalizesPath(t *testing.T) {
	a := WorkspaceHash("/Users/alice/projects/foo")
	b := WorkspaceHash("/Users/alice/projects/foo/")
	c := WorkspaceHash("/Users/alice/projects/foo/../foo")
	if a != b || a != c {
		t.Errorf("normalization broken: %q %q %q", a, b, c)
	}
}

func TestWorkspaceHash_EmptyReturnsEmpty(t *testing.T) {
	if got := WorkspaceHash(""); got != "" {
		t.Errorf("empty input → %q, want \"\"", got)
	}
}

func TestWorkspaceHash_NoPathLeakage(t *testing.T) {
	// A 12-hex-char hash can't contain "/Users/" or "alice" — sanity check
	// that we never accidentally return the input.
	in := "/Users/alice/projects/foo"
	out := WorkspaceHash(in)
	for _, leaked := range []string{"/", "alice", "Users", "projects", "foo"} {
		if contains(out, leaked) {
			t.Errorf("hash %q leaks substring %q from input %q", out, leaked, in)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
