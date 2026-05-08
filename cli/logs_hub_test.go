package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLogsCmd_HubRender pins the render path for #322's hub.log mode.
// A representative fixture (one rpc entry, one lifecycle entry, one
// hook entry) renders one line per entry, each with HH:MM:SS prefix
// and the structured fields surfaced as key=value pairs.
//
// The decodeEvent helper used by readLog does not know about hub.log's
// {ts, level, event, rpc, agent, ...} shape specifically — it lifts
// every non-reserved key into Fields. The renderer formats Fields
// alphabetically, so the agent/level/rpc tokens always appear in a
// stable order.
func TestLogsCmd_HubRender(t *testing.T) {
	tmp := t.TempDir()
	hubLog := filepath.Join(tmp, ".bones", "hub.log")
	if err := os.MkdirAll(filepath.Dir(hubLog), 0o755); err != nil {
		t.Fatal(err)
	}

	// Three lines of representative hub.log content. The timestamps
	// are pinned so the test does not depend on time.Now's zone or
	// drift; the renderer converts to display form which is local
	// time but here we just check substring shape.
	fixture := []string{
		`{"ts":"2026-05-08T12:00:00Z","level":"INFO",` +
			`"event":"lifecycle","msg":"hub: starting (pid=42)"}`,
		`{"ts":"2026-05-08T12:00:01Z","level":"INFO",` +
			`"event":"rpc","rpc":"tasks.create","agent":"claude-1",` +
			`"took_ms":12}`,
		`{"ts":"2026-05-08T12:00:02Z","level":"INFO",` +
			`"event":"hook","hook":"SessionStart","session":"abc123",` +
			`"msg":"primed 3"}`,
	}
	if err := os.WriteFile(hubLog, []byte(strings.Join(fixture, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	c := &LogsCmd{Hub: true}
	if err := readLog(hubLog, c, time.Time{}, false, &buf); err != nil {
		t.Fatalf("readLog: %v", err)
	}

	out := buf.String()
	lines := splitLines(out)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), out)
	}

	// Each entry's typed fields must appear somewhere in the rendered
	// output. The renderer puts level/event/rpc/etc. through the
	// Fields key=value path.
	for _, want := range []string{
		"hub: starting (pid=42)",
		"tasks.create",
		"claude-1",
		"SessionStart",
		"abc123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q.\nout:\n%s", want, out)
		}
	}
}

// TestLogsCmd_HubJSONPassthrough verifies --json --hub emits the raw
// NDJSON line so jq pipelines work on hub.log without re-parsing the
// human render.
func TestLogsCmd_HubJSONPassthrough(t *testing.T) {
	tmp := t.TempDir()
	hubLog := filepath.Join(tmp, ".bones", "hub.log")
	if err := os.MkdirAll(filepath.Dir(hubLog), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"ts":"2026-05-08T12:00:00Z","level":"INFO",` +
		`"event":"rpc","rpc":"tasks.create","agent":"claude-1",` +
		`"took_ms":7}`
	if err := os.WriteFile(hubLog, []byte(raw+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	c := &LogsCmd{Hub: true, JSON: true}
	if err := readLog(hubLog, c, time.Time{}, false, &buf); err != nil {
		t.Fatalf("readLog: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != raw {
		t.Errorf("--json --hub passthrough mismatch:\nraw: %s\ngot: %s", raw, got)
	}
}
