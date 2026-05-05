package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/registry"
)

// mkBonesDir creates the .bones/ directory inside root so the
// agent.id helper can write into it.
func mkBonesDir(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedTwoWorkspaces writes two registry entries (each with its own
// .bones/agent.id) and returns the resolved cwds.
func seedTwoWorkspaces(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	wsA := t.TempDir()
	wsB := t.TempDir()
	mkBonesDir(t, wsA)
	mkBonesDir(t, wsB)
	writeAgentIDForTest(t, wsA)
	writeAgentIDForTest(t, wsB)

	now := time.Now().UTC().Truncate(time.Second)
	if err := registry.Write(registry.Entry{
		Cwd: wsA, Name: "alpha",
		HubURL:    "http://127.0.0.1:1",
		NATSURL:   "nats://127.0.0.1:1",
		HubPID:    -1,
		StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Write(registry.Entry{
		Cwd: wsB, Name: "beta",
		HubURL:    "http://127.0.0.1:2",
		NATSURL:   "nats://127.0.0.1:2",
		HubPID:    -2,
		StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return wsA, wsB
}

// TestWorkspacesLsTable exercises the human path. We assert structural
// features (header line, two rows, names, hub-status column = "stopped")
// rather than the full byte sequence, because tabwriter padding is
// width-sensitive and home-shortening depends on $HOME.
func TestWorkspacesLsTable(t *testing.T) {
	seedTwoWorkspaces(t)

	infos, err := registry.ListInfo()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	// Pin "now" so humanRelative output is deterministic.
	now := infos[0].LastTouched.Add(2 * time.Minute)
	if err := writeWorkspacesTable(&buf, infos, now); err != nil {
		t.Fatalf("writeWorkspacesTable: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"NAME", "CWD", "HUB", "LAST-TOUCHED",
		"alpha", "beta", "stopped", "2 minutes ago",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// TestWorkspacesLsTableEmpty: empty registry → friendly hint.
func TestWorkspacesLsTableEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var buf bytes.Buffer
	if err := writeWorkspacesTable(&buf, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No workspaces registered") {
		t.Errorf("expected hint, got %q", buf.String())
	}
}

// TestWorkspacesLsJSON: JSON path emits the documented schema for both
// workspaces. We assert each top-level field on the alpha row to keep
// it tied to the spec; the beta row only needs to exist.
func TestWorkspacesLsJSON(t *testing.T) {
	wsA, _ := seedTwoWorkspaces(t)

	infos, err := registry.ListInfo()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeWorkspacesJSON(&buf, infos); err != nil {
		t.Fatalf("writeWorkspacesJSON: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// alpha sorts first; verify schema on it.
	want := map[string]string{
		"name":       "alpha",
		"cwd":        wsA,
		"hub_status": "stopped",
		"nats_url":   "nats://127.0.0.1:1",
		"hub_url":    "http://127.0.0.1:1",
		"agent_id":   "x", // writeAgentIDForTest writes literal "x"
	}
	for k, v := range want {
		if got, _ := rows[0][k].(string); got != v {
			t.Errorf("alpha[%q] = %q, want %q", k, got, v)
		}
	}
	id, _ := rows[0]["id"].(string)
	if id != registry.WorkspaceID(wsA) {
		t.Errorf("alpha.id = %q, want %q", id, registry.WorkspaceID(wsA))
	}
	if _, err := time.Parse(time.RFC3339, rows[0]["last_touched"].(string)); err != nil {
		t.Errorf("alpha.last_touched not RFC3339: %v", err)
	}
}

// TestWorkspacesShowByName: exact name match emits the matching entry's
// JSON shape.
func TestWorkspacesShowByName(t *testing.T) {
	seedTwoWorkspaces(t)
	infos, err := registry.ListInfo()
	if err != nil {
		t.Fatal(err)
	}
	matches := matchWorkspaces(infos, "beta")
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	var buf bytes.Buffer
	if err := writeWorkspaceOne(&buf, matches[0], true); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name"] != "beta" {
		t.Errorf("name = %v, want beta", got["name"])
	}
}

// TestWorkspacesShowByID: ID (filename hex) matches even when it does
// not match Name. This is the disambiguation handle for collisions.
func TestWorkspacesShowByID(t *testing.T) {
	wsA, _ := seedTwoWorkspaces(t)
	infos, err := registry.ListInfo()
	if err != nil {
		t.Fatal(err)
	}
	id := registry.WorkspaceID(wsA)
	matches := matchWorkspaces(infos, id)
	if len(matches) != 1 || matches[0].Cwd != wsA {
		t.Fatalf("matches = %+v, want 1 with cwd %s", matches, wsA)
	}
}

// TestWorkspacesShowAmbiguous: two workspaces sharing a Name yield a
// disambiguation error listing both IDs and CWDs.
func TestWorkspacesShowAmbiguous(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wsA := t.TempDir()
	wsB := t.TempDir()
	mkBonesDir(t, wsA)
	mkBonesDir(t, wsB)
	writeAgentIDForTest(t, wsA)
	writeAgentIDForTest(t, wsB)
	now := time.Now().UTC().Truncate(time.Second)
	for _, cwd := range []string{wsA, wsB} {
		if err := registry.Write(registry.Entry{
			Cwd: cwd, Name: "twin",
			HubURL: "http://127.0.0.1:0", HubPID: -1,
			StartedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	infos, _ := registry.ListInfo()
	matches := matchWorkspaces(infos, "twin")
	if len(matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(matches))
	}
	err := ambiguousNameError("twin", matches)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"matches 2 workspaces",
		registry.WorkspaceID(wsA),
		registry.WorkspaceID(wsB),
		wsA, wsB,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in error: %s", want, msg)
		}
	}
}

// TestWorkspacesShowNoMatch: unknown name yields a helpful error.
func TestWorkspacesShowNoMatch(t *testing.T) {
	seedTwoWorkspaces(t)
	infos, _ := registry.ListInfo()
	matches := matchWorkspaces(infos, "ghost")
	if len(matches) != 0 {
		t.Fatalf("expected zero matches, got %+v", matches)
	}
}

// TestWorkspacesLsJSONFixture: snapshot the JSON shape against a
// hand-curated fixture. We patch volatile field values (id, cwd,
// last_touched) by regex before comparing so the assertion is robust
// across hosts and clocks. Field ORDER is part of the spec — the
// fixture is the source of truth and writeWorkspacesJSON emits via
// a struct, so order is stable.
func TestWorkspacesLsJSONFixture(t *testing.T) {
	wsA, wsB := seedTwoWorkspaces(t)

	infos, err := registry.ListInfo()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeWorkspacesJSON(&buf, infos); err != nil {
		t.Fatal(err)
	}
	got := scrubVolatile(buf.String(), wsA, wsB,
		registry.WorkspaceID(wsA), registry.WorkspaceID(wsB))

	fixturePath := filepath.Join("testdata", "workspaces_ls.json")
	wantBytes, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	want := strings.TrimRight(string(wantBytes), "\n")
	got = strings.TrimRight(got, "\n")
	if got != want {
		t.Errorf("fixture mismatch:\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// scrubVolatile rewrites the JSON output so non-deterministic fields
// (id hash, absolute cwd, RFC3339 timestamp) are replaced with
// placeholder strings matching the fixture.
func scrubVolatile(s, wsA, wsB, idA, idB string) string {
	s = strings.ReplaceAll(s, idA, "<id>")
	s = strings.ReplaceAll(s, idB, "<id>")
	s = strings.ReplaceAll(s, wsA, "<cwd>")
	s = strings.ReplaceAll(s, wsB, "<cwd>")
	// last_touched is a quoted RFC3339 string; rewrite the whole
	// value via the field name.
	out := make([]string, 0, 16)
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, `"last_touched":`) {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			line = indent + `"last_touched": "<time>",`
			// Last item before "agent_id" — keep trailing comma.
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestRunPrune_YesRemovesStopped covers the non-interactive flow (#180):
// two seeded entries, both reported as HubStatus=stopped (HubPID=-1
// fails IsAlive), are prune-eligible. With yes=true the command
// removes both and the registry directory is empty afterwards.
//
// We also assert that pruning a workspace whose HubURL is empty (and
// thus reports HubStatus=unknown) is NOT touched — the contract for
// --prune is "stopped only". A separate seed exercises that case so
// the test fails if a regression starts pruning unknown entries.
func TestRunPrune_YesRemovesStopped(t *testing.T) {
	wsA, wsB := seedTwoWorkspaces(t)
	wsC := t.TempDir()
	mkBonesDir(t, wsC)
	writeAgentIDForTest(t, wsC)
	if err := registry.Write(registry.Entry{
		Cwd: wsC, Name: "unknown",
		HubURL: "", NATSURL: "", HubPID: 0,
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	infos, err := registry.ListInfo()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 3 {
		t.Fatalf("seed: have %d infos, want 3", len(infos))
	}

	var buf bytes.Buffer
	if err := runPrune(&buf, strings.NewReader(""), infos, true); err != nil {
		t.Fatalf("runPrune: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"pruned: alpha", "pruned: beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pruned: unknown") {
		t.Errorf("unknown-status entry should NOT be pruned:\n%s", out)
	}

	after, err := registry.ListInfo()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Cwd != wsC {
		t.Errorf("after prune: want only wsC=%s, got %+v", wsC, after)
	}
	for i, cwd := range []string{wsA, wsB} {
		// HubPID values seeded by seedTwoWorkspaces are -1 (wsA) and -2 (wsB).
		pid := -(i + 1)
		if _, err := os.Stat(registry.EntryPath(cwd, pid)); !os.IsNotExist(err) {
			t.Errorf("entry file for %s should be removed; err=%v", cwd, err)
		}
	}
}

// TestRunPrune_NoConfirmKeepsEntries covers the prompt-rejected path:
// the operator types "n" (or anything that is not "y"/"yes"), and the
// registry is left untouched. Exercises the "list-with-confirm" branch
// of the spec — the most common interactive flow.
func TestRunPrune_NoConfirmKeepsEntries(t *testing.T) {
	wsA, wsB := seedTwoWorkspaces(t)
	infos, err := registry.ListInfo()
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runPrune(&buf, strings.NewReader("n\n"), infos, false); err != nil {
		t.Fatalf("runPrune: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "2 stopped entries") {
		t.Errorf("missing dead-entry summary in output:\n%s", out)
	}
	if !strings.Contains(out, "Prune 2 stopped entries? [y/N]") {
		t.Errorf("missing prompt in output:\n%s", out)
	}
	if !strings.Contains(out, "aborted; no entries pruned") {
		t.Errorf("missing aborted-line in output:\n%s", out)
	}
	if strings.Contains(out, "pruned:") {
		t.Errorf("output should not contain any pruned: lines:\n%s", out)
	}

	for i, cwd := range []string{wsA, wsB} {
		pid := -(i + 1)
		if _, err := os.Stat(registry.EntryPath(cwd, pid)); err != nil {
			t.Errorf("entry for %s should still exist after abort; err=%v", cwd, err)
		}
	}
}

// TestRunPrune_EmptyRegistryNoOp pins the friendly-no-op path. An empty
// registry must exit 0 with a clear message rather than an error.
func TestRunPrune_EmptyRegistryNoOp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	infos, err := registry.ListInfo()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := runPrune(&buf, strings.NewReader(""), infos, true); err != nil {
		t.Fatalf("runPrune: %v", err)
	}
	if !strings.Contains(buf.String(), "no stopped entries to prune") {
		t.Errorf("missing friendly no-op line:\n%s", buf.String())
	}
}
