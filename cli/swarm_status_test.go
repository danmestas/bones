package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/dispatch"
)

// TestEmitDispatchLine_NoManifest verifies that emitDispatchLine is silent
// when no manifest exists.
func TestEmitDispatchLine_NoManifest(t *testing.T) {
	root := t.TempDir()

	// Capture stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	emitErr := emitDispatchLine(root)

	if err := w.Close(); err != nil {
		t.Fatalf("close write pipe: %v", err)
	}
	os.Stdout = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy: %v", err)
	}

	if emitErr != nil {
		t.Fatalf("emitDispatchLine with no manifest: %v", emitErr)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output with ErrNoManifest; got %q", buf.String())
	}
}

// TestEmitDispatchLine_WithManifest verifies that emitDispatchLine prints
// "Dispatch: <plan_path>  (wave N of M)" when a manifest is present, and
// that the line appears before any other output.
func TestEmitDispatchLine_WithManifest(t *testing.T) {
	root := t.TempDir()

	now := time.Now().UTC()
	m := dispatch.Manifest{
		SchemaVersion: dispatch.SchemaVersion,
		PlanPath:      "/tmp/plans/my-plan.md",
		PlanSHA256:    "abc123",
		CreatedAt:     now,
		CurrentWave:   1,
		Waves: []dispatch.Wave{
			{Wave: 1, Slots: []dispatch.SlotEntry{{Slot: "alpha", TaskID: "t1", Title: "Alpha"}}},
			{Wave: 2, Slots: []dispatch.SlotEntry{{Slot: "beta", TaskID: "t2", Title: "Beta"}}},
		},
	}
	if err := dispatch.Write(root, m); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Capture stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	emitErr := emitDispatchLine(root)

	if err := w.Close(); err != nil {
		t.Fatalf("close write pipe: %v", err)
	}
	os.Stdout = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy: %v", err)
	}

	if emitErr != nil {
		t.Fatalf("emitDispatchLine: %v", emitErr)
	}

	out := buf.String()
	want := fmt.Sprintf("Dispatch: %s  (wave %d of %d)\n", m.PlanPath, m.CurrentWave, len(m.Waves))
	if !strings.HasPrefix(out, want) {
		t.Errorf("output = %q\nwant prefix %q", out, want)
	}
}

// TestBuildStatusRows_Empty verifies buildStatusRows returns an empty slice
// for zero sessions.
func TestBuildStatusRows_Empty(t *testing.T) {
	rows := buildStatusRows(nil, "host", time.Now())
	if len(rows) != 0 {
		t.Errorf("expected 0 rows; got %d", len(rows))
	}
}
