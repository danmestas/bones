package integration_test

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCLI_Swarm exercises the full bones swarm join → commit → close
// cycle against a tmpdir workspace + a Go-implemented hub on
// dynamic ports. The test is skipped under `go test -short` to keep
// fast loops focused on unit tests.
//
// Layout:
//
//	tmp/
//	├── .git/                  initialized so hub seedRepo finds tracked files
//	├── seed.txt               one tracked file
//	├── .bones/                created by `bones init` (workspace leaf)
//	├── .orchestrator/         created by `bones hub start`
//	└── .bones/swarm/<slot>/   created by `bones swarm join`
func TestCLI_Swarm(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	requireBinaries(t)
	dir := setupSwarmWorkspace(t)
	// Resolve any /private/var prefix on macOS so the cwd assertion
	// matches what `bones swarm cwd` prints (it derives from
	// workspace.Join which calls filepath.EvalSymlinks).
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	httpPort, natsPort := pickPortPair(t)
	startSwarmHub(t, dir, httpPort, natsPort)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	// Coord.Claim → holds.Announce requires absolute file paths
	// (invariant 4). The slot's relative-to-wt content path is
	// "rendering/hello.txt"; the task records it as the absolute
	// workspace path so the hold's lock key is global.
	relFile := "rendering/hello.txt"
	absFile := filepath.Join(dir, relFile)
	taskID := createSwarmTask(t, dir, absFile, "[slot: rendering] render a thing")
	slot := "rendering"

	t.Run("join_creates_session_and_worktree", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, bonesBin, dir,
			"swarm", "join",
			"--slot="+slot, "--task-id="+taskID,
			"--hub-url="+hubURL,
		)
		if code != 0 {
			t.Fatalf("swarm join exit=%d stdout=%s stderr=%s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "BONES_SLOT_WT=") {
			t.Errorf("stdout missing BONES_SLOT_WT: %q", stdout)
		}
		// Verify worktree path exists.
		wt := filepath.Join(dir, ".bones", "swarm", slot, "wt")
		if _, err := os.Stat(wt); err != nil {
			t.Errorf("worktree %s missing: %v", wt, err)
		}
	})

	t.Run("status_lists_session", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, bonesBin, dir, "swarm", "status", "--json")
		if code != 0 {
			t.Fatalf("swarm status exit=%d stderr=%s", code, stderr)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
			t.Fatalf("parse json: %v\nout=%s", err, stdout)
		}
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d: %s", len(rows), stdout)
		}
		if rows[0]["slot"] != slot {
			t.Errorf("slot mismatch: %v", rows[0])
		}
	})

	t.Run("cwd_prints_worktree_path", func(t *testing.T) {
		stdout, _, code := runCmd(t, bonesBin, dir, "swarm", "cwd", "--slot="+slot)
		if code != 0 {
			t.Fatalf("swarm cwd exit=%d", code)
		}
		got := strings.TrimSpace(stdout)
		want := filepath.Join(dir, ".bones", "swarm", slot, "wt")
		if got != want {
			t.Errorf("cwd: got %q want %q", got, want)
		}
	})

	t.Run("commit_writes_a_file", func(t *testing.T) {
		wt := filepath.Join(dir, ".bones", "swarm", slot, "wt")
		// libfossil's checkout open is part of commit; for now we
		// stage a fresh file in the wt and pass it explicitly.
		filePath := filepath.Join(wt, "rendering", "hello.txt")
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			t.Fatalf("mkdir wt subdir: %v", err)
		}
		if err := os.WriteFile(filePath, []byte("hello swarm\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		stdout, stderr, code := runCmd(t, bonesBin, dir,
			"swarm", "commit",
			"--slot="+slot, "-m=add hello",
			"--hub-url="+hubURL,
			"rendering/hello.txt",
		)
		if code != 0 {
			t.Fatalf("swarm commit exit=%d stderr=%s", code, stderr)
		}
		// First non-empty line of stdout should be the commit UUID.
		uuid := firstLine(stdout)
		if len(uuid) < 16 {
			t.Errorf("expected commit uuid, got %q", stdout)
		}
		// Hub-side propagation: the commit's UUID must land in the
		// hub repo's timeline. Without the post-commit HTTP push the
		// commit only lives in the slot's leaf.fossil — `bones peek`
		// and any hub consumer would see nothing. This is the
		// regression guard for the swarm-commit-hub-sync fix.
		//
		// Phase 1 ignores the -m flag at the libfossil layer (the
		// leaf hardcodes a "leaf commit for task <id>" message), so
		// we assert on the commit UUID prefix instead, which is
		// stable and cross-version.
		hubRepoPath := filepath.Join(dir, ".orchestrator", "hub.fossil")
		tlOut, err := exec.Command("fossil",
			"timeline", "-R", hubRepoPath, "-n", "5", "-t", "ci",
		).CombinedOutput()
		if err != nil {
			t.Fatalf("fossil timeline -R %s: %v\n%s", hubRepoPath, err, tlOut)
		}
		// fossil timeline shortens UUIDs to 10 chars; compare on the prefix.
		uuidPrefix := uuid
		if len(uuidPrefix) > 10 {
			uuidPrefix = uuidPrefix[:10]
		}
		if !strings.Contains(string(tlOut), uuidPrefix) {
			t.Fatalf("commit %s not in hub timeline; commit-stderr=%s\ntimeline:\n%s",
				uuid, stderr, tlOut)
		}
	})

	t.Run("close_deletes_session_and_closes_task", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, bonesBin, dir,
			"swarm", "close",
			"--slot="+slot, "--result=success",
			"--summary=test done",
			"--hub-url="+hubURL,
		)
		if code != 0 {
			t.Fatalf("swarm close exit=%d stderr=%s stdout=%s", code, stderr, stdout)
		}
		// Session should be gone.
		statusOut, _, sCode := runCmd(t, bonesBin, dir, "swarm", "status", "--json")
		if sCode != 0 {
			t.Fatalf("post-close status code=%d", sCode)
		}
		// JSON null or empty array — both acceptable representations of "no rows."
		trimmed := strings.TrimSpace(statusOut)
		if trimmed != "null" && trimmed != "[]" {
			t.Errorf("status post-close: want empty, got %q", statusOut)
		}
		// Task should be closed.
		showOut, _, _ := runCmd(t, bonesBin, dir, "tasks", "show", taskID)
		if !strings.Contains(showOut, "status=closed") {
			t.Errorf("task post-close: want status=closed, got\n%s", showOut)
		}
	})
}

// setupSwarmWorkspace creates a git-initialized tmpdir with a single
// tracked file so the Go-implemented hub's seedHubRepo finds something
// to commit. Then runs `bones init` to bring up the workspace leaf.
func setupSwarmWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Init a fresh git repo and stage one file. Hub's seedHubRepo
	// walks `git ls-files` and refuses to seed an empty workspace.
	gitInit := exec.Command("git", "init", "-q", "-b", "main")
	gitInit.Dir = dir
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed.txt: %v", err)
	}
	// Configure committer for this fixture so `git commit` doesn't
	// pull in the operator's config.
	for _, args := range [][]string{
		{"config", "user.email", "swarm-test@example.invalid"},
		{"config", "user.name", "Swarm Test"},
		{"add", "seed.txt"},
		{"commit", "-q", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// `bones init` to spawn the leaf daemon.
	if _, stderr, code := runCmd(t, bonesBin, dir, "init"); code != 0 {
		t.Fatalf("bones init: %s", stderr)
	}
	t.Cleanup(func() {
		killPidFile(t, filepath.Join(dir, ".bones", "leaf.pid"))
	})
	return dir
}

// startSwarmHub launches `bones hub start --detach` on the given
// ports and registers cleanup to stop it.
func startSwarmHub(t *testing.T, dir string, httpPort, natsPort int) {
	t.Helper()
	args := []string{
		"hub", "start", "--detach",
		"--fossil-port=" + strconv.Itoa(httpPort),
		"--nats-port=" + strconv.Itoa(natsPort),
	}
	stdout, stderr, code := runCmd(t, bonesBin, dir, args...)
	if code != 0 {
		t.Fatalf("hub start exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	t.Cleanup(func() {
		// Best-effort: signal SIGTERM via the hub's pid files.
		for _, name := range []string{"fossil.pid", "nats.pid"} {
			pidPath := filepath.Join(dir, ".orchestrator", "pids", name)
			data, err := os.ReadFile(pidPath)
			if err != nil {
				continue
			}
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if proc, err := os.FindProcess(pid); err == nil {
					_ = proc.Signal(syscall.SIGTERM)
				}
			}
		}
		// Wait briefly for graceful shutdown.
		time.Sleep(100 * time.Millisecond)
	})
}

// pickPortPair returns two free localhost TCP ports for the hub.
// The kernel may assign them, then we close before the hub binds —
// races are possible but rare; the hub start surfaces a clear error
// if it fails to bind.
func pickPortPair(t *testing.T) (httpPort, natsPort int) {
	t.Helper()
	httpPort = grabFreePort(t)
	natsPort = grabFreePort(t)
	return
}

func grabFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// createSwarmTask creates a task with the given title and absolute
// file path, then returns the new task id. Caller passes an absolute
// file path so coord.Claim → holds.Announce passes the IsAbs gate.
func createSwarmTask(t *testing.T, dir, absFile, title string) string {
	t.Helper()
	stdout, stderr, code := runCmd(t, bonesBin, dir, "tasks", "create",
		"--files="+absFile,
		"--context", "slot=rendering",
		title)
	if code != 0 {
		t.Fatalf("tasks create exit=%d stderr=%s", code, stderr)
	}
	id := firstLine(stdout)
	if len(id) < 16 {
		t.Fatalf("expected uuid on stdout, got %q", stdout)
	}
	return id
}

// createSwarmTaskNoFiles creates a task with NO --files= and only a
// --slot= context. Mirrors the orchestrator-skill flow where agents
// create per-slot tasks without enumerating files up front; the slot
// owns its wt/ and `swarm commit` should announce holds + auto-
// discover files at commit time.
func createSwarmTaskNoFiles(t *testing.T, dir, slot, title string) string {
	t.Helper()
	stdout, stderr, code := runCmd(t, bonesBin, dir, "tasks", "create",
		"--context", "slot="+slot,
		title)
	if code != 0 {
		t.Fatalf("tasks create (no files) exit=%d stderr=%s", code, stderr)
	}
	id := firstLine(stdout)
	if len(id) < 16 {
		t.Fatalf("expected uuid on stdout, got %q", stdout)
	}
	return id
}

// TestCLI_SwarmAutoDiscover exercises the no-pre-populated-files
// path: a task created with --slot= but NO --files=, joined,
// populated with two new files in wt/, then committed via `swarm
// commit -m "..."` with NO positional file args. Auto-discovery
// must pick up both files and the commit must land in the hub
// timeline. Regression guard for the two gaps surfaced by the
// 2026-04-28 swarm-demo retro.
func TestCLI_SwarmAutoDiscover(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	requireBinaries(t)
	dir := setupSwarmWorkspace(t)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	httpPort, natsPort := pickPortPair(t)
	startSwarmHub(t, dir, httpPort, natsPort)
	hubURL := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	slot := "rendering"
	taskID := createSwarmTaskNoFiles(t, dir, slot, "[slot: rendering] no-files task")

	// Join.
	if _, stderr, code := runCmd(t, bonesBin, dir,
		"swarm", "join",
		"--slot="+slot, "--task-id="+taskID,
		"--hub-url="+hubURL,
	); code != 0 {
		t.Fatalf("swarm join exit=%d stderr=%s", code, stderr)
	}

	// Populate wt/ with two files. Both untracked.
	wt := filepath.Join(dir, ".bones", "swarm", slot, "wt")
	files := map[string][]byte{
		filepath.Join(wt, "out", "a.txt"):      []byte("first auto file\n"),
		filepath.Join(wt, "out", "b", "c.txt"): []byte("second auto file\n"),
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir wt subdir: %v", err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// Commit with NO file args — auto-discovery should pick up both.
	stdout, stderr, code := runCmd(t, bonesBin, dir,
		"swarm", "commit",
		"--slot="+slot, "-m=auto-discover both files",
		"--hub-url="+hubURL,
	)
	if code != 0 {
		t.Fatalf("swarm commit (auto) exit=%d stderr=%s stdout=%s", code, stderr, stdout)
	}
	uuid := firstLine(stdout)
	if len(uuid) < 16 {
		t.Fatalf("expected commit uuid on stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "files=2") {
		t.Errorf("stderr should report files=2 from auto-discover; got: %s", stderr)
	}

	// Hub timeline must carry the commit.
	hubRepoPath := filepath.Join(dir, ".orchestrator", "hub.fossil")
	tlOut, err := exec.Command("fossil",
		"timeline", "-R", hubRepoPath, "-n", "5", "-t", "ci",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("fossil timeline -R %s: %v\n%s", hubRepoPath, err, tlOut)
	}
	uuidPrefix := uuid
	if len(uuidPrefix) > 10 {
		uuidPrefix = uuidPrefix[:10]
	}
	if !strings.Contains(string(tlOut), uuidPrefix) {
		t.Fatalf("commit %s not in hub timeline; commit-stderr=%s\ntimeline:\n%s",
			uuid, stderr, tlOut)
	}

	// Verify both files landed in the manifest. `fossil artifact
	// <uuid>` dumps the raw checkin manifest; the F-cards list
	// every file with its name and content hash. We grep for the
	// workspace-relative tail because the in-fossil filename is
	// the disk-absolute path with the leading slash trimmed (per
	// gatherFiles' Path mapping — the hold-bucket key is the
	// abs path, and Leaf.Commit reuses File.Path as the commit
	// filename via normalizeLeadingSlash).
	manifestOut, mErr := exec.Command("fossil",
		"artifact", "-R", hubRepoPath, uuid,
	).CombinedOutput()
	if mErr != nil {
		t.Fatalf("fossil artifact -R %s %s: %v\n%s",
			hubRepoPath, uuid, mErr, manifestOut)
	}
	for _, want := range []string{"out/a.txt", "out/b/c.txt"} {
		if !strings.Contains(string(manifestOut), want) {
			t.Errorf("hub commit missing %q; artifact:\n%s", want, manifestOut)
		}
	}

	// Clean shutdown.
	if _, stderr, code := runCmd(t, bonesBin, dir,
		"swarm", "close",
		"--slot="+slot, "--result=success", "--summary=auto",
		"--hub-url="+hubURL,
	); code != 0 {
		t.Fatalf("swarm close exit=%d stderr=%s", code, stderr)
	}
}
