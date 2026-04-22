package main_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// binPath resolves the agent-tasks binary (absolute) so cmd.Dir changes don't break it.
var binPath = func() string {
	if p := os.Getenv("AGENT_TASKS_BIN"); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	abs, err := filepath.Abs("../../bin/agent-tasks")
	if err != nil {
		return "../../bin/agent-tasks"
	}
	return abs
}()

// agentInitBin resolves agent-init similarly — tests need it to bootstrap a workspace.
var agentInitBin = func() string {
	if p := os.Getenv("AGENT_INIT_BIN"); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	abs, err := filepath.Abs("../../bin/agent-init")
	if err != nil {
		return "../../bin/agent-init"
	}
	return abs
}()

func leafBinary() string {
	if p := os.Getenv("LEAF_BIN"); p != "" {
		return p
	}
	return "leaf"
}

func requireBinaries(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("agent-tasks binary not built (%v); run `make agent-tasks`", err)
	}
	if _, err := os.Stat(agentInitBin); err != nil {
		t.Skipf("agent-init binary not built (%v); run `make agent-init`", err)
	}
	if _, err := exec.LookPath(leafBinary()); err != nil {
		t.Skipf("leaf binary not available (%v); set LEAF_BIN", err)
	}
}

func runCmd(t *testing.T, bin, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runCmdEnv(t, bin, dir, append(os.Environ(), "LEAF_BIN="+leafBinary()), args...)
}

func runCmdEnv(
	t *testing.T, bin, dir string, env []string, args ...string,
) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("run %s %v: %v", bin, args, err)
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

func killPidFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGKILL)
	}
}

// newWorkspace bootstraps a workspace in a tmpdir and returns it. The caller
// registers killPidFile cleanup itself via t.Cleanup (done once per test).
func newWorkspace(t *testing.T) string {
	t.Helper()
	requireBinaries(t)
	dir := t.TempDir()
	t.Cleanup(func() { killPidFile(t, filepath.Join(dir, ".agent-infra", "leaf.pid")) })
	if _, stderr, code := runCmd(t, agentInitBin, dir, "init"); code != 0 {
		t.Fatalf("init failed: %s", stderr)
	}
	return dir
}

// firstLine returns the first non-empty line of s (trimmed).
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func TestCLI_Create(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	t.Run("basic", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "create", "my first task")
		if code != 0 {
			t.Fatalf("create exit=%d stderr=%s", code, stderr)
		}
		id := firstLine(stdout)
		if len(id) < 16 {
			t.Errorf("expected UUID on stdout, got %q", stdout)
		}
	})

	t.Run("with_flags", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "create",
			"--files=a.go,b.go",
			"--context", "source=manual",
			"--context", "owner=dan",
			"task with metadata")
		if code != 0 {
			t.Fatalf("create exit=%d stderr=%s", code, stderr)
		}
		if firstLine(stdout) == "" {
			t.Error("expected id on stdout")
		}
	})

	t.Run("missing_title", func(t *testing.T) {
		_, stderr, code := runCmd(t, binPath, dir, "create")
		if code != 1 {
			t.Errorf("exit=%d, want 1", code)
		}
		if !strings.Contains(stderr, "title") {
			t.Errorf("stderr should mention title: %q", stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "create", "--json", "json task")
		if code != 0 {
			t.Fatalf("create --json exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, `"id":`) || !strings.Contains(stdout, `"title":"json task"`) {
			t.Errorf("json output missing fields: %q", stdout)
		}
	})
}

func TestCLI_Show(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	// Seed a task
	createOut, _, code := runCmd(t, binPath, dir, "create", "show me")
	if code != 0 {
		t.Fatalf("seed create failed: code=%d", code)
	}
	id := firstLine(createOut)

	t.Run("exists", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "show", id)
		if code != 0 {
			t.Fatalf("show exit=%d stderr=%s", code, stderr)
		}
		for _, sub := range []string{"id=" + id, "title=show me", "status=open"} {
			if !strings.Contains(stdout, sub) {
				t.Errorf("show stdout missing %q; got:\n%s", sub, stdout)
			}
		}
	})

	t.Run("missing_id_exits_6", func(t *testing.T) {
		_, _, code := runCmd(t, binPath, dir, "show", "00000000-0000-0000-0000-000000000000")
		if code != 6 {
			t.Errorf("exit=%d, want 6", code)
		}
	})

	t.Run("json", func(t *testing.T) {
		stdout, _, code := runCmd(t, binPath, dir, "show", "--json", id)
		if code != 0 {
			t.Fatalf("show --json failed code=%d", code)
		}
		if !strings.Contains(stdout, `"id":"`+id+`"`) {
			t.Errorf("json output missing id: %q", stdout)
		}
	})
}

func TestCLI_List(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	// Seed three tasks; we'll mutate the last one via update in Task 8 tests —
	// for now just create three open tasks.
	var ids []string
	for _, title := range []string{"first", "second", "third"} {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		ids = append(ids, firstLine(out))
	}

	t.Run("default_excludes_nothing_here_yet", func(t *testing.T) {
		// With no closed tasks, default list should show all 3.
		stdout, stderr, code := runCmd(t, binPath, dir, "list")
		if code != 0 {
			t.Fatalf("list exit=%d stderr=%s", code, stderr)
		}
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if len(lines) != 3 {
			t.Errorf("expected 3 lines, got %d:\n%s", len(lines), stdout)
		}
		for _, id := range ids {
			if !strings.Contains(stdout, id) {
				t.Errorf("list missing id %s", id)
			}
		}
	})

	t.Run("status_open_filter", func(t *testing.T) {
		stdout, _, code := runCmd(t, binPath, dir, "list", "--status=open")
		if code != 0 {
			t.Fatalf("list --status=open failed code=%d", code)
		}
		if strings.Count(stdout, "\n") != 3 {
			t.Errorf("expected 3 lines for open filter, got:\n%s", stdout)
		}
	})

	t.Run("status_invalid_exits_1", func(t *testing.T) {
		_, stderr, code := runCmd(t, binPath, dir, "list", "--status=bogus")
		if code != 1 {
			t.Errorf("exit=%d, want 1 (usage error)", code)
		}
		if !strings.Contains(stderr, "invalid status") {
			t.Errorf("stderr should flag invalid status: %q", stderr)
		}
	})

	t.Run("claimed_by_unclaimed", func(t *testing.T) {
		stdout, _, code := runCmd(t, binPath, dir, "list", "--claimed-by=-")
		if code != 0 {
			t.Fatalf("list --claimed-by=- failed code=%d", code)
		}
		if strings.Count(stdout, "\n") != 3 {
			t.Errorf("expected all 3 unclaimed, got:\n%s", stdout)
		}
	})

	t.Run("json", func(t *testing.T) {
		stdout, _, code := runCmd(t, binPath, dir, "list", "--json")
		if code != 0 {
			t.Fatalf("list --json failed code=%d", code)
		}
		if !strings.HasPrefix(strings.TrimSpace(stdout), "[") {
			t.Errorf("json output should be an array, got: %q", stdout)
		}
	})
}

func TestCLI_Update(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	seed := func(title string) string {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		return firstLine(out)
	}

	t.Run("title", func(t *testing.T) {
		id := seed("old title")
		_, stderr, code := runCmd(t, binPath, dir, "update", id, "--title=new title")
		if code != 0 {
			t.Fatalf("update --title failed code=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "title=new title") {
			t.Errorf("title not updated; show:\n%s", stdout)
		}
	})

	t.Run("context_merge", func(t *testing.T) {
		id := seed("ctx test")
		runCmd(t, binPath, dir, "update", id, "--context", "k1=v1")
		runCmd(t, binPath, dir, "update", id, "--context", "k2=v2")
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "context.k1=v1") ||
			!strings.Contains(stdout, "context.k2=v2") {
			t.Errorf("merge failed; show:\n%s", stdout)
		}
	})

	t.Run("claimed_by", func(t *testing.T) {
		id := seed("claimed via update")
		_, stderr, code := runCmd(t, binPath, dir, "update", id, "--claimed-by=other-agent")
		if code != 0 {
			t.Fatalf("update --claimed-by failed code=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "claimed_by=other-agent") {
			t.Errorf("claimed_by not set; show:\n%s", stdout)
		}
		// Invariant 11: setting claimed_by must auto-transition status to claimed.
		if !strings.Contains(stdout, "status=claimed") {
			t.Errorf("status did not auto-transition to claimed; show:\n%s", stdout)
		}
	})

	t.Run("claimed_by_clear", func(t *testing.T) {
		id := seed("claim then release")
		// First claim it.
		_, stderr, code := runCmd(t, binPath, dir, "update", id, "--claimed-by=someone")
		if code != 0 {
			t.Fatalf("update --claimed-by=someone failed code=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "status=claimed") {
			t.Fatalf("setup: status did not auto-transition to claimed; show:\n%s", stdout)
		}
		// Then clear the claim — status should revert to open.
		_, stderr, code = runCmd(t, binPath, dir, "update", id, "--claimed-by=")
		if code != 0 {
			t.Fatalf("update --claimed-by= failed code=%d stderr=%s", code, stderr)
		}
		stdout, _, _ = runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "status=open") {
			t.Errorf("status did not revert to open after clearing claimed_by; show:\n%s", stdout)
		}
		if strings.Contains(stdout, "claimed_by=someone") {
			t.Errorf("claimed_by still present after clear; show:\n%s", stdout)
		}
	})

	t.Run("invalid_status_exits_1", func(t *testing.T) {
		id := seed("bad status")
		_, stderr, code := runCmd(t, binPath, dir, "update", id, "--status=bogus")
		if code != 1 {
			t.Errorf("exit=%d, want 1", code)
		}
		if !strings.Contains(stderr, "invalid status") {
			t.Errorf("stderr should flag invalid status: %q", stderr)
		}
	})
}

func TestCLI_Claim(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	seed := func(title string) string {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		return firstLine(out)
	}

	t.Run("happy_path", func(t *testing.T) {
		id := seed("claim me")
		_, stderr, code := runCmd(t, binPath, dir, "claim", id)
		if code != 0 {
			t.Fatalf("claim exit=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "status=claimed") {
			t.Errorf("status not claimed; show:\n%s", stdout)
		}
		if !strings.Contains(stdout, "claimed_by=") {
			t.Errorf("claimed_by not set; show:\n%s", stdout)
		}
	})

	t.Run("idempotent_same_agent", func(t *testing.T) {
		id := seed("claim twice")
		runCmd(t, binPath, dir, "claim", id)
		_, stderr, code := runCmd(t, binPath, dir, "claim", id)
		if code != 0 {
			t.Fatalf("re-claim should be no-op, got exit=%d stderr=%s", code, stderr)
		}
	})

	t.Run("conflict_other_agent_exits_7", func(t *testing.T) {
		id := seed("foreign")
		// Steal via update as a different agent id.
		if _, stderr, code := runCmd(t, binPath, dir, "update", id,
			"--status=claimed", "--claimed-by=foreign-agent"); code != 0 {
			t.Fatalf("seed foreign claim failed code=%d stderr=%s", code, stderr)
		}
		_, stderr, code := runCmd(t, binPath, dir, "claim", id)
		if code != 7 {
			t.Errorf("exit=%d, want 7 (claim conflict)", code)
		}
		if !strings.Contains(stderr, "already claimed") {
			t.Errorf("stderr should mention already claimed: %q", stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		id := seed("json claim")
		stdout, _, code := runCmd(t, binPath, dir, "claim", "--json", id)
		if code != 0 {
			t.Fatalf("claim --json failed code=%d", code)
		}
		if !strings.Contains(stdout, `"status":"claimed"`) {
			t.Errorf("json missing claimed status: %q", stdout)
		}
	})
}

func TestCLI_Close(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	seed := func(title string) string {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		return firstLine(out)
	}

	t.Run("basic", func(t *testing.T) {
		id := seed("close me")
		_, stderr, code := runCmd(t, binPath, dir, "close", id)
		if code != 0 {
			t.Fatalf("close exit=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "status=closed") {
			t.Errorf("status not closed; show:\n%s", stdout)
		}
		if !strings.Contains(stdout, "closed_at=") {
			t.Errorf("closed_at not set; show:\n%s", stdout)
		}
		if !strings.Contains(stdout, "closed_by=") {
			t.Errorf("closed_by not set; show:\n%s", stdout)
		}
	})

	t.Run("reason", func(t *testing.T) {
		id := seed("close with reason")
		_, _, code := runCmd(t, binPath, dir, "close", id, "--reason=canceled by user")
		if code != 0 {
			t.Fatalf("close --reason failed code=%d", code)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "closed_reason=canceled by user") {
			t.Errorf("closed_reason not set; show:\n%s", stdout)
		}
	})

	t.Run("hidden_from_default_list", func(t *testing.T) {
		id := seed("will be hidden")
		runCmd(t, binPath, dir, "close", id)
		stdout, _, _ := runCmd(t, binPath, dir, "list")
		if strings.Contains(stdout, id) {
			t.Errorf("closed task should be excluded from default list; got:\n%s", stdout)
		}
		all, _, _ := runCmd(t, binPath, dir, "list", "--all")
		if !strings.Contains(all, id) {
			t.Errorf("--all should include closed task; got:\n%s", all)
		}
	})

	t.Run("json", func(t *testing.T) {
		id := seed("json close")
		stdout, _, code := runCmd(t, binPath, dir, "close", "--json", id)
		if code != 0 {
			t.Fatalf("close --json failed code=%d", code)
		}
		if !strings.Contains(stdout, `"status":"closed"`) {
			t.Errorf("json missing closed status: %q", stdout)
		}
	})
}

func TestCLI_Ready(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	seed := func(title string) string {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		return firstLine(out)
	}

	t.Run("empty_bucket", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "ready")
		if code != 0 {
			t.Fatalf("ready exit=%d stderr=%s", code, stderr)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Errorf("expected empty output, got: %q", stdout)
		}
	})

	t.Run("shows_open_tasks", func(t *testing.T) {
		id := seed("ready task")
		stdout, stderr, code := runCmd(t, binPath, dir, "ready")
		if code != 0 {
			t.Fatalf("ready exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, id) {
			t.Errorf("ready missing task %s; got:\n%s", id, stdout)
		}
	})

	t.Run("hides_claimed_tasks", func(t *testing.T) {
		id := seed("claimed task")
		_, _, code := runCmd(t, binPath, dir, "claim", id)
		if code != 0 {
			t.Fatalf("seed claim failed code=%d", code)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "ready")
		if strings.Contains(stdout, id) {
			t.Errorf("claimed task should be hidden; got:\n%s", stdout)
		}
	})

	t.Run("json", func(t *testing.T) {
		id := seed("json ready")
		stdout, stderr, code := runCmd(t, binPath, dir, "ready", "--json")
		if code != 0 {
			t.Fatalf("ready --json exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, `"id":"`+id+`"`) {
			t.Errorf("json missing id: %q", stdout)
		}
	})
}

func TestCLI_AutoClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	seed := func(title string) string {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		return firstLine(out)
	}

	t.Run("disabled_by_env", func(t *testing.T) {
		stdout, stderr, code := runCmdEnv(t, binPath, dir,
			append(os.Environ(), "LEAF_BIN="+leafBinary(), "AGENT_INFRA_AUTOCLAIM=0"),
			"autoclaim")
		if code != 0 {
			t.Fatalf("autoclaim exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, "action=disabled") {
			t.Fatalf("stdout=%q, want disabled result", stdout)
		}
	})

	t.Run("claims_one_ready_task", func(t *testing.T) {
		id := seed("auto task")
		stdout, stderr, code := runCmd(t, binPath, dir, "autoclaim", "--idle=true")
		if code != 0 {
			t.Fatalf("autoclaim exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, "action=claimed") {
			t.Fatalf("stdout=%q, want claimed action", stdout)
		}
		show, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(show, "status=claimed") {
			t.Fatalf("show=%q, want claimed status", show)
		}
	})

	t.Run("busy_noop", func(t *testing.T) {
		seed("busy task")
		stdout, stderr, code := runCmd(t, binPath, dir, "autoclaim", "--idle=false")
		if code != 0 {
			t.Fatalf("autoclaim exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, "action=busy") {
			t.Fatalf("stdout=%q, want busy action", stdout)
		}
	})
}

func TestCLI_Dispatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	seed := func(title string) string {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		return firstLine(out)
	}

	t.Run("requires_mode", func(t *testing.T) {
		_, stderr, code := runCmd(t, binPath, dir, "dispatch")
		if code != 1 {
			t.Fatalf("exit=%d, want 1", code)
		}
		if !strings.Contains(stderr, "parent|worker") {
			t.Fatalf("stderr=%q", stderr)
		}
	})

	t.Run("worker_posts_progress", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir,
			"dispatch", "worker",
			"--task-id=agent-infra-placeholder",
			"--task-thread=agent-infra-placeholder",
			"--worker-agent-id=parent-agent/agent-infra-placeholder",
			"--result=success",
			"--summary=done",
		)
		if code != 0 {
			t.Fatalf("worker exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, "posted progress") {
			t.Fatalf("stdout=%q", stdout)
		}
	})

	t.Run("parent_spawns_worker_for_claimed_task", func(t *testing.T) {
		id := seed("dispatch task")
		_, _, code := runCmd(t, binPath, dir, "claim", id)
		if code != 0 {
			t.Fatalf("claim failed: %d", code)
		}
		stdout, stderr, code := runCmd(t, binPath, dir,
			"dispatch", "parent",
			"--task-id="+id,
			"--worker-bin="+binPath,
			"--worker-result=success",
			"--worker-summary=done",
		)
		if code != 0 {
			t.Fatalf("parent dispatch exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, "closed") {
			t.Fatalf("stdout=%q", stdout)
		}
		show, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(show, "status=closed") {
			t.Fatalf("show=%q, want closed status", show)
		}
	})
}

func TestCLI_Link(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	seed := func(title string) string {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		return firstLine(out)
	}

	t.Run("blocks_hides_target_from_ready", func(t *testing.T) {
		from := seed("blocker")
		to := seed("blocked")
		_, stderr, code := runCmd(t, binPath, dir, "link",
			from, to, "--type=blocks")
		if code != 0 {
			t.Fatalf("link exit=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "ready")
		if strings.Contains(stdout, to) {
			t.Errorf("blocked target should be hidden; got:\n%s", stdout)
		}
		if !strings.Contains(stdout, from) {
			t.Errorf("blocker should still be ready; got:\n%s", stdout)
		}
	})

	t.Run("supersedes_hides_target", func(t *testing.T) {
		from := seed("winner")
		to := seed("loser")
		_, stderr, code := runCmd(t, binPath, dir, "link",
			from, to, "--type=supersedes")
		if code != 0 {
			t.Fatalf("link exit=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "ready")
		if strings.Contains(stdout, to) {
			t.Errorf("superseded task should be hidden; got:\n%s", stdout)
		}
	})

	t.Run("discovered_from_does_not_hide", func(t *testing.T) {
		from := seed("discovery")
		to := seed("seed")
		_, stderr, code := runCmd(t, binPath, dir, "link",
			from, to, "--type=discovered-from")
		if code != 0 {
			t.Fatalf("link exit=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "ready")
		if !strings.Contains(stdout, to) {
			t.Errorf("discovered-from should not hide target; got:\n%s", stdout)
		}
		if !strings.Contains(stdout, from) {
			t.Errorf("discovering task should still be ready; got:\n%s", stdout)
		}
	})

	t.Run("missing_type", func(t *testing.T) {
		from := seed("no type")
		to := seed("target")
		_, stderr, code := runCmd(t, binPath, dir, "link", from, to)
		if code != 1 {
			t.Errorf("exit=%d, want 1", code)
		}
		if !strings.Contains(stderr, "--type") {
			t.Errorf("stderr should mention --type: %q", stderr)
		}
	})

	t.Run("invalid_type", func(t *testing.T) {
		from := seed("bad type")
		to := seed("target")
		_, stderr, code := runCmd(t, binPath, dir, "link",
			from, to, "--type=bogus")
		if code != 1 {
			t.Errorf("exit=%d, want 1", code)
		}
		if !strings.Contains(stderr, "invalid edge type") {
			t.Errorf("stderr should flag invalid type: %q", stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		from := seed("json link")
		to := seed("json target")
		stdout, stderr, code := runCmd(t, binPath, dir, "link",
			from, to, "--type=blocks", "--json")
		if code != 0 {
			t.Fatalf("link --json exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, `"from":"`+from+`"`) {
			t.Errorf("json missing from: %q", stdout)
		}
	})
}
