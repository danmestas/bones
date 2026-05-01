package registry

import "testing"

func TestWorkspaceID(t *testing.T) {
	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"simple path", "/Users/dan/projects/foo", "726a213943fe1d41"},
		{"trailing slash normalized", "/Users/dan/projects/foo/", "726a213943fe1d41"},
		{"different path", "/Users/dan/projects/bar", "45675b631d01125f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorkspaceID(tt.cwd)
			if len(got) != 16 {
				t.Fatalf("WorkspaceID(%q) length = %d, want 16", tt.cwd, len(got))
			}
			// Same path always produces same ID
			if got2 := WorkspaceID(tt.cwd); got != got2 {
				t.Fatalf("WorkspaceID not deterministic: %q vs %q", got, got2)
			}
		})
	}
	// Different paths produce different IDs
	a := WorkspaceID("/a")
	b := WorkspaceID("/b")
	if a == b {
		t.Fatalf("WorkspaceID collision: /a and /b both = %q", a)
	}
}
