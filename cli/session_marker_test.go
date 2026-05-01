package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/bones/internal/sessions"
)

func TestSessionMarkerRegister(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := SessionMarkerRegisterCmd{
		SessionID: "test-sid",
		Cwd:       "/Users/dan/projects/foo",
		PID:       os.Getpid(),
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	path := filepath.Join(os.Getenv("HOME"), ".bones", "sessions", cmd.SessionID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected marker at %s: %v", path, err)
	}
	if got := sessions.ListByWorkspace(cmd.Cwd); len(got) != 1 {
		t.Fatalf("ListByWorkspace = %d markers, want 1", len(got))
	}
}

func TestSessionMarkerUnregister(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := SessionMarkerRegisterCmd{SessionID: "to-remove", Cwd: "/x", PID: os.Getpid()}
	if err := reg.Run(); err != nil {
		t.Fatalf("Register: %v", err)
	}
	un := SessionMarkerUnregisterCmd{SessionID: "to-remove"}
	if err := un.Run(); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if _, err := os.Stat(sessions.MarkerPath("to-remove")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker still exists after Unregister")
	}
}
