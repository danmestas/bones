package main

import (
	"reflect"
	"testing"
)

// TestDetectHelpAll covers the pre-parse os.Args filter that intercepts
// `--help --all`. The detector must:
//   - return helpAll=false (and untouched args) when --all is absent.
//   - return helpAll=false when --all is present but no help flag is.
//   - return helpAll=true and strip --all when both --help and --all (or
//     -h and --all) are present.
func TestDetectHelpAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantArgs []string
		wantAll  bool
	}{
		{
			name:     "no flags",
			args:     []string{"bones"},
			wantArgs: []string{"bones"},
			wantAll:  false,
		},
		{
			name:     "help only",
			args:     []string{"bones", "--help"},
			wantArgs: []string{"bones", "--help"},
			wantAll:  false,
		},
		{
			name: "all without help (e.g. tasks list --all)",
			args: []string{"bones", "tasks", "list", "--all"},
			// --all here is the user-facing tasks-list flag; we must
			// not strip it because no help flag is present.
			wantArgs: []string{"bones", "tasks", "list", "--all"},
			wantAll:  false,
		},
		{
			name:     "help + all, long form",
			args:     []string{"bones", "--help", "--all"},
			wantArgs: []string{"bones", "--help"},
			wantAll:  true,
		},
		{
			name:     "help + all, short help",
			args:     []string{"bones", "-h", "--all"},
			wantArgs: []string{"bones", "-h"},
			wantAll:  true,
		},
		{
			name:     "verb + help + all",
			args:     []string{"bones", "tasks", "--help", "--all"},
			wantArgs: []string{"bones", "tasks", "--help"},
			wantAll:  true,
		},
		{
			name:     "all before help",
			args:     []string{"bones", "--all", "--help"},
			wantArgs: []string{"bones", "--help"},
			wantAll:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotArgs, gotAll := detectHelpAll(tt.args)
			if gotAll != tt.wantAll {
				t.Errorf("helpAll = %v, want %v", gotAll, tt.wantAll)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Errorf("filtered args = %v, want %v", gotArgs, tt.wantArgs)
			}
		})
	}
}
