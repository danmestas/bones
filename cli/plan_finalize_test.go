package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	edgehub "github.com/danmestas/EdgeSync/hub"
	libfossil "github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/modernc"

	"github.com/danmestas/bones/internal/dispatch"
)

// reopenAsHubRepo closes a libfossil.Repo and reopens it as an
// edgehub.Repo so the test can call materializeManifest with the
// migrated signature. Test setup uses libfossil directly because
// the test scope hasn't migrated; production code now uses
// edgehub.OpenRepo end-to-end.
func reopenAsHubRepo(t *testing.T, repo *libfossil.Repo, path string) *edgehub.Repo {
	t.Helper()
	if err := repo.Close(); err != nil {
		t.Fatalf("close libfossil repo: %v", err)
	}
	r, err := edgehub.OpenRepo(path)
	if err != nil {
		t.Fatalf("edgehub.OpenRepo: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// TestResolvePlanManifest_NoSourceErrors pins the no-source case:
// without --plan and without dispatch.json, the verb errors with
// the explicit message ADR 0044 mandates.
func TestResolvePlanManifest_NoSourceErrors(t *testing.T) {
	dir := t.TempDir()
	_, _, err := resolvePlanManifest("", dir)
	if err == nil {
		t.Fatal("expected error when no plan source")
	}
	if !strings.Contains(err.Error(), "no active dispatch found") {
		t.Errorf("error missing required phrase, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--plan=") {
		t.Errorf("error missing --plan hint, got: %v", err)
	}
}

// TestMaterializeManifest_WritesMatchesConflicts exercises the four
// outcome categories of the materialization pass against a real
// libfossil-backed hub trunk.
func TestMaterializeManifest_WritesMatchesConflicts(t *testing.T) {
	workspaceDir := t.TempDir()
	bonesDir := filepath.Join(workspaceDir, ".bones")
	if err := os.MkdirAll(bonesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hubRepoPath := filepath.Join(bonesDir, "hub.fossil")
	repo, err := libfossil.Create(hubRepoPath, libfossil.CreateOpts{User: "hub"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	// Seed hub trunk with three files: one will be "written" (no host
	// counterpart), one "matched" (host already equals trunk), and one
	// "conflicted" (host differs from trunk).
	files := []libfossil.FileToCommit{
		{Name: "docs/research/rendering/REPORT.md", Content: []byte("rendering report\n")},
		{Name: "docs/research/physics/REPORT.md", Content: []byte("physics report\n")},
		{Name: "docs/research/ui/REPORT.md", Content: []byte("ui report v2\n")},
	}
	if _, _, err := repo.Commit(libfossil.CommitOpts{
		Files:   files,
		Comment: "seed",
		User:    "hub",
		Time:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Pre-populate host tree:
	//   - rendering: nothing yet → expect "written"
	//   - physics: same content as trunk → expect "matched"
	//   - ui: different content → expect "conflicted" (no force)
	mustWrite(t, workspaceDir, "docs/research/physics/REPORT.md", "physics report\n")
	mustWrite(t, workspaceDir, "docs/research/ui/REPORT.md", "ui report OLD\n")

	manifest := &dispatch.Manifest{
		Waves: []dispatch.Wave{
			{
				Wave: 1,
				Slots: []dispatch.SlotEntry{
					{Slot: "rendering", Files: []string{
						"docs/research/rendering/REPORT.md",
					}},
					{Slot: "physics", Files: []string{
						"docs/research/physics/REPORT.md",
					}},
					{Slot: "ui", Files: []string{
						"docs/research/ui/REPORT.md",
					}},
				},
			},
		},
	}

	hubRepo := reopenAsHubRepo(t, repo, hubRepoPath)
	res := materializeManifest(context.Background(), hubRepo, manifest, workspaceDir, false)

	if !equalSorted(res.Written, []string{"docs/research/rendering/REPORT.md"}) {
		t.Errorf("Written: got %v", res.Written)
	}
	if !equalSorted(res.Matched, []string{"docs/research/physics/REPORT.md"}) {
		t.Errorf("Matched: got %v", res.Matched)
	}
	if !equalSorted(res.Conflicted, []string{"docs/research/ui/REPORT.md"}) {
		t.Errorf("Conflicted: got %v", res.Conflicted)
	}

	// Conflicted file's host content must NOT have been overwritten.
	got, _ := os.ReadFile(filepath.Join(workspaceDir, "docs/research/ui/REPORT.md"))
	if string(got) != "ui report OLD\n" {
		t.Errorf("conflicted file overwritten without --force: %q", got)
	}

	// Written file's content must match trunk.
	got, _ = os.ReadFile(filepath.Join(workspaceDir, "docs/research/rendering/REPORT.md"))
	if string(got) != "rendering report\n" {
		t.Errorf("written file content: %q", got)
	}
}

// TestMaterializeManifest_ForceOverridesConflicts pins --force: the
// conflicting host file is overwritten with trunk content.
func TestMaterializeManifest_ForceOverridesConflicts(t *testing.T) {
	workspaceDir := t.TempDir()
	hubRepoPath := filepath.Join(workspaceDir, ".bones", "hub.fossil")
	if err := os.MkdirAll(filepath.Dir(hubRepoPath), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := libfossil.Create(hubRepoPath, libfossil.CreateOpts{User: "hub"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if _, _, err := repo.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: "out.md", Content: []byte("trunk version\n")},
		},
		Comment: "seed", User: "hub", Time: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	mustWrite(t, workspaceDir, "out.md", "host version (older)\n")

	manifest := &dispatch.Manifest{Waves: []dispatch.Wave{{
		Wave: 1, Slots: []dispatch.SlotEntry{
			{Slot: "alpha", Files: []string{"out.md"}},
		},
	}}}
	hubRepo := reopenAsHubRepo(t, repo, hubRepoPath)
	res := materializeManifest(context.Background(), hubRepo, manifest, workspaceDir, true)

	if len(res.Conflicted) != 0 {
		t.Errorf("expected no conflicts under --force, got %v", res.Conflicted)
	}
	if !equalSorted(res.Written, []string{"out.md"}) {
		t.Errorf("Written: got %v", res.Written)
	}
	got, _ := os.ReadFile(filepath.Join(workspaceDir, "out.md"))
	if string(got) != "trunk version\n" {
		t.Errorf("force did not overwrite: %q", got)
	}
}

// TestMaterializeManifest_MissingOnTrunk pins the recovery path: a
// dispatch manifest references a file that wasn't actually committed
// to trunk. The file is reported as missing, not as a hard error.
func TestMaterializeManifest_MissingOnTrunk(t *testing.T) {
	workspaceDir := t.TempDir()
	hubRepoPath := filepath.Join(workspaceDir, ".bones", "hub.fossil")
	if err := os.MkdirAll(filepath.Dir(hubRepoPath), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := libfossil.Create(hubRepoPath, libfossil.CreateOpts{User: "hub"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	// Empty repo — no commits. The "expected file" is not on trunk.
	manifest := &dispatch.Manifest{Waves: []dispatch.Wave{{
		Wave: 1, Slots: []dispatch.SlotEntry{
			{Slot: "ghost", Files: []string{"never-committed.md"}},
		},
	}}}
	hubRepo := reopenAsHubRepo(t, repo, hubRepoPath)
	res := materializeManifest(context.Background(), hubRepo, manifest, workspaceDir, false)

	if !equalSorted(res.Missing, []string{"never-committed.md"}) {
		t.Errorf("Missing: got %v", res.Missing)
	}
	if len(res.Written)+len(res.Matched)+len(res.Conflicted) != 0 {
		t.Errorf("non-missing categories should be empty, got %+v", res)
	}
}

// TestPrintFinalizeSummary_StableShape pins the per-category section
// labels even when categories are empty — keeps the output shape
// stable for operators scanning logs.
func TestPrintFinalizeSummary_StableShape(t *testing.T) {
	var buf bytes.Buffer
	res := finalizeResult{
		Written:    []string{"a"},
		Matched:    nil,
		Conflicted: []string{"b"},
	}
	printFinalizeSummary(&buf, res)
	out := buf.String()
	for _, want := range []string{
		"written (1):", "  - a",
		"matched (0):", "    (none)",
		"conflicted (1):", "  - b",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// mustWrite is a tmpdir helper: create the parent directory and write
// content. Failures fail the test immediately.
func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// equalSorted reports element-wise equality after sort. Both slices
// are mutated in place; callers don't need the original ordering.
func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
