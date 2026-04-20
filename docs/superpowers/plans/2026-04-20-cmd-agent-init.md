# cmd/agent-init Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a CLI binary `agent-init` with two subcommands (`init`, `join`) that create or reconnect to an agent-infra workspace backed by a real `leaf` daemon subprocess.

**Architecture:** Single deep package `internal/workspace` exposes `Init(ctx, cwd) (Info, error)` and `Join(ctx, cwd) (Info, error)`. All filesystem, subprocess, and HTTP work is hidden behind that two-function surface. `cmd/agent-init/main.go` is a thin dispatcher that maps sentinel errors to exit codes. Tests use real processes, real ports, and real fossil repos — no mocks.

**Tech Stack:** Go 1.26, stdlib `encoding/json`, `net`, `os/exec`, `net/http`, `log/slog`; `github.com/google/uuid` (already transitive); `github.com/danmestas/libfossil` for repo creation; `github.com/dmestas/edgesync/leaf/telemetry` for OTel setup.

**Spec:** `docs/superpowers/specs/2026-04-20-cmd-agent-init-design.md`

---

## File Structure

Files created by this plan:

```
internal/workspace/
    workspace.go          # exported Init, Join, Info, error sentinels
    workspace_test.go     # TestInit_*, TestJoin_*, exercises public API + unexported helpers
    walk.go               # unexported walkUp helper
    config.go             # unexported Config type + loadConfig/saveConfig
    spawn.go              # unexported spawnLeaf + waitHealthz + writePID
    verify.go             # unexported pidAlive + healthzOK

cmd/agent-init/
    main.go               # flag parsing, dispatch, exit-code mapping, telemetry bootstrap
    integration_test.go   # spawns the built agent-init binary, exercises exit-code contract

Makefile                  # add `agent-init` target + ensure `leaf` is available for tests
```

Each file has one clear responsibility. Internal helpers live in unexported symbols (lowercase) so the public surface stays exactly the two functions plus `Info` and error sentinels.

---

## Task 1: Scaffold package + public surface

Establish the compile-clean skeleton so later tasks only add logic.

**Files:**
- Create: `internal/workspace/workspace.go`
- Create: `internal/workspace/workspace_test.go`

- [ ] **Step 1: Create workspace.go with public types and stubs**

```go
// Package workspace manages an agent-infra workspace: the .agent-infra/
// directory, its on-disk config, and the associated leaf daemon process.
//
// Two entry points:
//
//	Init creates a fresh workspace and starts a leaf daemon.
//	Join locates an existing workspace (walking up from cwd) and verifies
//	its leaf is reachable.
package workspace

import (
	"context"
	"errors"
)

// Info describes a live workspace. Returned by both Init and Join.
type Info struct {
	AgentID      string
	NATSURL      string
	LeafHTTPURL  string
	RepoPath     string
	WorkspaceDir string
}

var (
	ErrAlreadyInitialized = errors.New("workspace already initialized")
	ErrNoWorkspace        = errors.New("no agent-infra workspace found")
	ErrLeafUnreachable    = errors.New("leaf daemon not reachable")
	ErrLeafStartTimeout   = errors.New("leaf daemon failed to start within timeout")
)

// Init creates a fresh workspace rooted at cwd, starts a leaf daemon, and
// returns its connection info. Returns ErrAlreadyInitialized if .agent-infra/
// already exists in cwd.
func Init(ctx context.Context, cwd string) (Info, error) {
	return Info{}, errors.New("not implemented")
}

// Join locates the nearest .agent-infra/ walking up from cwd and verifies
// the recorded leaf is still reachable.
func Join(ctx context.Context, cwd string) (Info, error) {
	return Info{}, errors.New("not implemented")
}
```

- [ ] **Step 2: Create workspace_test.go with a build-canary test**

```go
package workspace

import "testing"

func TestPackageBuilds(t *testing.T) {
	// Sanity: exported symbols compile and sentinel errors are distinct.
	errs := []error{ErrAlreadyInitialized, ErrNoWorkspace, ErrLeafUnreachable, ErrLeafStartTimeout}
	seen := map[error]bool{}
	for _, e := range errs {
		if e == nil {
			t.Fatal("nil sentinel")
		}
		if seen[e] {
			t.Fatalf("duplicate sentinel: %v", e)
		}
		seen[e] = true
	}
}
```

- [ ] **Step 3: Run tests to verify scaffold compiles and canary passes**

Run: `go test ./internal/workspace/ -run TestPackageBuilds -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/workspace/workspace.go internal/workspace/workspace_test.go
git commit -m "feat(workspace): scaffold package with public surface (zh8)"
```

---

## Task 2: Config — JSON schema + load/save

Workspace config is the stable boundary between `init` and `join`. Build and test it in isolation.

**Files:**
- Create: `internal/workspace/config.go`
- Modify: `internal/workspace/workspace_test.go` (append `TestConfig_*` tests)

- [ ] **Step 1: Write failing tests**

Append to `internal/workspace/workspace_test.go`:

```go
func TestConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	orig := config{
		Version:     configVersion,
		AgentID:     "agent-123",
		NATSURL:     "nats://127.0.0.1:4222",
		LeafHTTPURL: "http://127.0.0.1:51234",
		RepoPath:    "repo.fossil",
		CreatedAt:   "2026-04-20T14:45:00Z",
	}
	path := dir + "/config.json"
	if err := saveConfig(path, orig); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	got, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got != orig {
		t.Fatalf("round-trip mismatch:\n got:  %+v\n want: %+v", got, orig)
	}
}

func TestConfig_RejectsUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("expected error for unknown version, got nil")
	}
}
```

Add `"os"` to the test file imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/workspace/ -run TestConfig -v`
Expected: FAIL — `undefined: config`, `undefined: saveConfig`, `undefined: loadConfig`, `undefined: configVersion`

- [ ] **Step 3: Create config.go**

```go
package workspace

import (
	"encoding/json"
	"fmt"
	"os"
)

const configVersion = 1

// config is the on-disk schema for .agent-infra/config.json.
// Fields are JSON-tagged for snake_case on disk; version gates schema migrations.
type config struct {
	Version     int    `json:"version"`
	AgentID     string `json:"agent_id"`
	NATSURL     string `json:"nats_url"`
	LeafHTTPURL string `json:"leaf_http_url"`
	RepoPath    string `json:"repo_path"`
	CreatedAt   string `json:"created_at"`
}

func saveConfig(path string, c config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("read config: %w", err)
	}
	var c config
	if err := json.Unmarshal(data, &c); err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
	if c.Version != configVersion {
		return config{}, fmt.Errorf("unsupported config version %d (expected %d)", c.Version, configVersion)
	}
	return c, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/workspace/ -run TestConfig -v`
Expected: PASS on both tests.

- [ ] **Step 5: Commit**

```bash
git add internal/workspace/config.go internal/workspace/workspace_test.go
git commit -m "feat(workspace): JSON config schema with version gate (zh8)"
```

---

## Task 3: Walk — find marker upward

Pure filesystem helper. Small but the failure modes (reach root, symlinks, cwd doesn't exist) are worth locking in early.

**Files:**
- Create: `internal/workspace/walk.go`
- Modify: `internal/workspace/workspace_test.go`

- [ ] **Step 1: Write failing tests**

Append to `workspace_test.go`:

```go
func TestWalk_FindsMarkerInCwd(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, markerDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := walkUp(dir)
	if err != nil {
		t.Fatalf("walkUp: %v", err)
	}
	if got != dir {
		t.Fatalf("walkUp: got %q, want %q", got, dir)
	}
}

func TestWalk_FindsMarkerInAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, markerDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := walkUp(deep)
	if err != nil {
		t.Fatalf("walkUp: %v", err)
	}
	if got != root {
		t.Fatalf("walkUp: got %q, want %q", got, root)
	}
}

func TestWalk_NoMarkerReturnsErrNoWorkspace(t *testing.T) {
	dir := t.TempDir()
	_, err := walkUp(dir)
	if !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("walkUp: got %v, want ErrNoWorkspace", err)
	}
}
```

Ensure imports include `"errors"`, `"os"`, `"path/filepath"`.

- [ ] **Step 2: Run tests — expect fail**

Run: `go test ./internal/workspace/ -run TestWalk -v`
Expected: FAIL — `undefined: walkUp`, `undefined: markerDirName`

- [ ] **Step 3: Create walk.go**

```go
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
)

const markerDirName = ".agent-infra"

// walkUp searches from start upward for a directory containing markerDirName.
// Returns the path of the directory containing it, or ErrNoWorkspace if the
// filesystem root is reached without finding one.
func walkUp(start string) (string, error) {
	cur, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	for {
		candidate := filepath.Join(cur, markerDirName)
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", ErrNoWorkspace
		}
		cur = parent
	}
}
```

- [ ] **Step 4: Run tests — expect pass**

Run: `go test ./internal/workspace/ -run TestWalk -v`
Expected: PASS on all three.

- [ ] **Step 5: Commit**

```bash
git add internal/workspace/walk.go internal/workspace/workspace_test.go
git commit -m "feat(workspace): walkUp finds .agent-infra/ in ancestor (zh8)"
```

---

## Task 4: Init — fresh workspace (mkdir + config + repo)

Implement the first half of `Init`: create `.agent-infra/`, write config, create fossil repo. Leaf spawn comes in Task 5.

**Files:**
- Modify: `internal/workspace/workspace.go`
- Modify: `internal/workspace/workspace_test.go`

- [ ] **Step 1: Write the partial-Init failing test**

Append to `workspace_test.go`:

```go
func TestInit_WritesConfigAndRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: creates fossil repo")
	}
	dir := t.TempDir()
	ctx := context.Background()

	// Temporarily redirect spawnLeaf to a no-op so this test focuses on
	// config + repo creation. Replace in Task 5 when full Init lands.
	savedSpawn := spawnLeafFunc
	spawnLeafFunc = func(ctx context.Context, _ spawnParams) (int, error) { return 0, nil }
	t.Cleanup(func() { spawnLeafFunc = savedSpawn })

	info, err := Init(ctx, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Marker dir present
	if _, err := os.Stat(filepath.Join(dir, markerDirName)); err != nil {
		t.Fatalf("marker dir missing: %v", err)
	}
	// Config round-trips
	cfg, err := loadConfig(filepath.Join(dir, markerDirName, "config.json"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AgentID == "" {
		t.Error("config.AgentID empty")
	}
	if info.AgentID != cfg.AgentID {
		t.Errorf("Info.AgentID %q != config %q", info.AgentID, cfg.AgentID)
	}
	// Fossil repo file exists
	if _, err := os.Stat(filepath.Join(dir, markerDirName, "repo.fossil")); err != nil {
		t.Fatalf("repo.fossil missing: %v", err)
	}
}
```

Add `"context"` to imports if not present.

- [ ] **Step 2: Run test — expect fail**

Run: `go test ./internal/workspace/ -run TestInit_WritesConfigAndRepo -v`
Expected: FAIL — `undefined: spawnLeafFunc`, `undefined: spawnParams`, and `Init` returns the stub error.

- [ ] **Step 3: Add the spawn seam and implement Init body**

Replace the `Init` stub in `workspace.go` and add imports:

```go
import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/modernc"
	"github.com/google/uuid"
)

// spawnParams is the input to spawnLeafFunc. Split out for test seams.
type spawnParams struct {
	LeafBinary string
	RepoPath   string
	HTTPAddr   string
	LogPath    string
}

// spawnLeafFunc is the production spawner. Tests replace it via a saved/restored
// pointer to isolate subprocess behavior (see TestInit_WritesConfigAndRepo).
var spawnLeafFunc = spawnLeaf

func Init(ctx context.Context, cwd string) (Info, error) {
	markerDir := filepath.Join(cwd, markerDirName)
	if _, err := os.Stat(markerDir); err == nil {
		return Info{}, ErrAlreadyInitialized
	} else if !errors.Is(err, os.ErrNotExist) {
		return Info{}, fmt.Errorf("stat marker: %w", err)
	}

	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("mkdir marker: %w", err)
	}

	httpPort, err := pickFreePort()
	if err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, fmt.Errorf("pick http port: %w", err)
	}

	repoPath := filepath.Join(markerDir, "repo.fossil")
	cfg := config{
		Version:     configVersion,
		AgentID:     uuid.NewString(),
		NATSURL:     "nats://127.0.0.1:4222",
		LeafHTTPURL: fmt.Sprintf("http://127.0.0.1:%d", httpPort),
		RepoPath:    repoPath,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := saveConfig(filepath.Join(markerDir, "config.json"), cfg); err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, err
	}

	repo, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: cfg.AgentID})
	if err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, fmt.Errorf("create fossil repo: %w", err)
	}
	_ = repo.Close()

	// Leaf spawn is exercised in Task 5. Bind to 127.0.0.1 only — we never
	// want this daemon reachable from outside localhost by default.
	_, err = spawnLeafFunc(ctx, spawnParams{
		LeafBinary: leafBinaryPath(),
		RepoPath:   repoPath,
		HTTPAddr:   fmt.Sprintf("127.0.0.1:%d", httpPort),
		LogPath:    filepath.Join(markerDir, "leaf.log"),
	})
	if err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, err
	}

	return Info{
		AgentID:      cfg.AgentID,
		NATSURL:      cfg.NATSURL,
		LeafHTTPURL:  cfg.LeafHTTPURL,
		RepoPath:     cfg.RepoPath,
		WorkspaceDir: cwd,
	}, nil
}

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// leafBinaryPath returns LEAF_BIN if set, else "leaf" (resolved via PATH).
func leafBinaryPath() string {
	if p := os.Getenv("LEAF_BIN"); p != "" {
		return p
	}
	return "leaf"
}

// spawnLeaf is a placeholder replaced in Task 5.
func spawnLeaf(ctx context.Context, p spawnParams) (int, error) {
	return 0, errors.New("spawnLeaf: not implemented")
}
```

- [ ] **Step 4: Run test — expect pass**

Run: `go test ./internal/workspace/ -run TestInit_WritesConfigAndRepo -v`
Expected: PASS. The test replaces `spawnLeafFunc` with a no-op so the placeholder error doesn't fire.

- [ ] **Step 5: Commit**

```bash
git add internal/workspace/workspace.go internal/workspace/workspace_test.go
git commit -m "feat(workspace): Init writes config + creates fossil repo (zh8)"
```

---

## Task 5: Init — spawn leaf + poll healthz + write pid

Complete `Init` by implementing `spawnLeaf` against a real `leaf` binary. This is the task where end-to-end behavior first works.

**Files:**
- Create: `internal/workspace/spawn.go`
- Modify: `internal/workspace/workspace.go`
- Modify: `internal/workspace/workspace_test.go`

- [ ] **Step 1: Write the end-to-end failing test**

Append to `workspace_test.go`:

```go
func TestInit_FreshDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf daemon")
	}
	requireLeafBinary(t)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := Init(ctx, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { killLeafPID(t, filepath.Join(dir, markerDirName, "leaf.pid")) })

	// Healthz reachable
	resp, err := http.Get(info.LeafHTTPURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status: got %d, want 200", resp.StatusCode)
	}

	// PID file written and process alive
	pidData, err := os.ReadFile(filepath.Join(dir, markerDirName, "leaf.pid"))
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("parse pid: %v", err)
	}
	if !pidAlive(pid) {
		t.Fatalf("leaf pid %d not alive", pid)
	}
}

// requireLeafBinary skips the test if leaf isn't on PATH or LEAF_BIN.
func requireLeafBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(leafBinaryPath()); err != nil {
		t.Skipf("leaf binary not available (%v); set LEAF_BIN or build it", err)
	}
}

// killLeafPID reads a pid file and sends SIGKILL. Best-effort for cleanup.
func killLeafPID(t *testing.T, pidPath string) {
	t.Helper()
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGKILL)
}
```

Ensure these test-file imports exist: `"net/http"`, `"os/exec"`, `"strconv"`, `"strings"`, `"syscall"`, `"time"`.

Also remove the spawn-seam override from `TestInit_WritesConfigAndRepo` — it's now redundant once real spawn works. Replace its body with `t.Skip("superseded by TestInit_FreshDir")` (keeps the name reserved) or delete it; keep the test name reserved to avoid plan drift. Recommended: delete.

- [ ] **Step 2: Run test — expect fail**

Run: `go test ./internal/workspace/ -run TestInit_FreshDir -v`
Expected: FAIL — `spawnLeaf: not implemented` or missing `pidAlive`.

- [ ] **Step 3: Create spawn.go with real implementation**

```go
package workspace

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const (
	healthzPollInterval = 50 * time.Millisecond
	healthzPollTimeout  = 2 * time.Second
)

// spawnLeaf execs the leaf binary, waits for /healthz to report 200, and
// writes its PID next to the workspace config. Returns the child PID.
// On any failure the child process is killed before returning.
func spawnLeaf(ctx context.Context, p spawnParams) (int, error) {
	logFile, err := os.OpenFile(p.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open leaf log: %w", err)
	}
	// Ownership note: logFile is inherited by the child; we close our copy after Start.

	cmd := exec.Command(p.LeafBinary,
		"--repo", p.RepoPath,
		"--serve-http", p.HTTPAddr,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return 0, fmt.Errorf("start leaf: %w", err)
	}
	_ = logFile.Close()

	pid := cmd.Process.Pid
	pidPath := filepath.Join(filepath.Dir(p.LogPath), "leaf.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		_ = cmd.Process.Kill()
		return 0, fmt.Errorf("write pid: %w", err)
	}

	healthzURL := "http://" + p.HTTPAddr + "/healthz"
	if err := waitHealthz(ctx, healthzURL); err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(pidPath)
		return 0, err
	}
	return pid, nil
}

// waitHealthz polls the given URL until it returns 200 or the timeout elapses.
// Returns ErrLeafStartTimeout on timeout.
func waitHealthz(ctx context.Context, url string) error {
	deadline := time.Now().Add(healthzPollTimeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	client := &http.Client{Timeout: healthzPollInterval}
	for {
		if time.Now().After(deadline) {
			return ErrLeafStartTimeout
		}
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ErrLeafStartTimeout
		case <-time.After(healthzPollInterval):
		}
	}
}
```

Also in `workspace.go` remove the placeholder `spawnLeaf` body (the one that returned "not implemented") since `spawn.go` now owns it. Keep `spawnLeafFunc = spawnLeaf` unchanged.

- [ ] **Step 4: Build leaf binary so test can find it**

Run: `(cd ../EdgeSync && make leaf)` then `export LEAF_BIN=$(realpath ../EdgeSync/bin/leaf)`

(If `make leaf` fails, document in the task note and surface to the user — the plan's integration story depends on this binary.)

- [ ] **Step 5: Run test — expect pass**

Run: `go test ./internal/workspace/ -run TestInit_FreshDir -v -timeout 60s`
Expected: PASS. Test leaves no leaf processes running (cleanup kills pid).

- [ ] **Step 6: Commit**

```bash
git add internal/workspace/spawn.go internal/workspace/workspace.go internal/workspace/workspace_test.go
git commit -m "feat(workspace): spawn leaf + healthz polling completes Init (zh8)"
```

---

## Task 6: Init — rollback + already-initialized guard

Two error paths on `Init`. Rollback was already wired in Task 4; lock it in with a test. `ErrAlreadyInitialized` was wired in Task 4; lock that in too.

**Files:**
- Modify: `internal/workspace/workspace_test.go`

- [ ] **Step 1: Write both failing tests**

Append to `workspace_test.go`:

```go
func TestInit_AlreadyInitialized(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf")
	}
	requireLeafBinary(t)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := Init(ctx, dir)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	t.Cleanup(func() { killLeafPID(t, filepath.Join(dir, markerDirName, "leaf.pid")) })

	_, err = Init(ctx, dir)
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Init: got %v, want ErrAlreadyInitialized", err)
	}

	// First workspace untouched: config still loads and matches info.
	cfg, err := loadConfig(filepath.Join(dir, markerDirName, "config.json"))
	if err != nil {
		t.Fatalf("loadConfig after second Init: %v", err)
	}
	if cfg.AgentID != info.AgentID {
		t.Errorf("agent id drifted: got %q, want %q", cfg.AgentID, info.AgentID)
	}
}

func TestInit_RollbackOnLeafFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf")
	}
	requireLeafBinary(t)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Force leaf to fail by making the binary path invalid via override.
	savedSpawn := spawnLeafFunc
	spawnLeafFunc = func(ctx context.Context, p spawnParams) (int, error) {
		return 0, ErrLeafStartTimeout
	}
	t.Cleanup(func() { spawnLeafFunc = savedSpawn })

	_, err := Init(ctx, dir)
	if !errors.Is(err, ErrLeafStartTimeout) {
		t.Fatalf("Init: got %v, want ErrLeafStartTimeout", err)
	}
	// Marker must be removed — no half-initialized state.
	if _, err := os.Stat(filepath.Join(dir, markerDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".agent-infra/ still exists after rollback: stat=%v", err)
	}
}
```

- [ ] **Step 2: Run tests — expect pass**

Run: `go test ./internal/workspace/ -run "TestInit_AlreadyInitialized|TestInit_RollbackOnLeafFailure" -v -timeout 60s`
Expected: PASS (behavior was already wired in Task 4; these tests ratify it).

- [ ] **Step 3: Commit**

```bash
git add internal/workspace/workspace_test.go
git commit -m "test(workspace): lock in Init rollback + already-initialized (zh8)"
```

---

## Task 7: Join — happy path + error paths

Implement `Join` end-to-end with its verification helpers. Covers `TestJoin_FromSubdir`, `TestJoin_NoMarker`, `TestJoin_StaleLeaf`.

**Files:**
- Create: `internal/workspace/verify.go`
- Modify: `internal/workspace/workspace.go`
- Modify: `internal/workspace/workspace_test.go`

- [ ] **Step 1: Write failing tests**

Append to `workspace_test.go`:

```go
func TestJoin_FromSubdir(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf")
	}
	requireLeafBinary(t)

	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	initInfo, err := Init(ctx, root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { killLeafPID(t, filepath.Join(root, markerDirName, "leaf.pid")) })

	subdir := filepath.Join(root, "deep", "nested", "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	joinInfo, err := Join(ctx, subdir)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if joinInfo.AgentID != initInfo.AgentID {
		t.Errorf("AgentID drift: init=%q join=%q", initInfo.AgentID, joinInfo.AgentID)
	}
	if joinInfo.LeafHTTPURL != initInfo.LeafHTTPURL {
		t.Errorf("LeafHTTPURL drift: init=%q join=%q", initInfo.LeafHTTPURL, joinInfo.LeafHTTPURL)
	}
	if joinInfo.WorkspaceDir != root {
		t.Errorf("WorkspaceDir: got %q, want %q", joinInfo.WorkspaceDir, root)
	}
}

func TestJoin_NoMarker(t *testing.T) {
	dir := t.TempDir()
	_, err := Join(context.Background(), dir)
	if !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("Join: got %v, want ErrNoWorkspace", err)
	}
}

func TestJoin_StaleLeaf(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf")
	}
	requireLeafBinary(t)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := Init(ctx, dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Kill the leaf, then try Join.
	killLeafPID(t, filepath.Join(dir, markerDirName, "leaf.pid"))

	// Give the OS a moment to reap.
	time.Sleep(100 * time.Millisecond)

	_, err := Join(ctx, dir)
	if !errors.Is(err, ErrLeafUnreachable) {
		t.Fatalf("Join: got %v, want ErrLeafUnreachable", err)
	}
}
```

- [ ] **Step 2: Run tests — expect fail**

Run: `go test ./internal/workspace/ -run TestJoin -v -timeout 60s`
Expected: FAIL — Join returns stub error.

- [ ] **Step 3: Create verify.go**

```go
package workspace

import (
	"net/http"
	"os"
	"syscall"
	"time"
)

// pidAlive returns true if a process with the given PID exists and accepts signal 0.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal(0) doesn't deliver a signal; it only checks existence/permissions.
	return proc.Signal(syscall.Signal(0)) == nil
}

// healthzOK returns true if the given URL returns HTTP 200 within the timeout.
func healthzOK(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}
```

- [ ] **Step 4: Implement Join in workspace.go**

Replace the `Join` stub with:

```go
func Join(ctx context.Context, cwd string) (Info, error) {
	workspaceDir, err := walkUp(cwd)
	if err != nil {
		return Info{}, err
	}
	cfg, err := loadConfig(filepath.Join(workspaceDir, markerDirName, "config.json"))
	if err != nil {
		return Info{}, fmt.Errorf("load config: %w", err)
	}
	pidData, err := os.ReadFile(filepath.Join(workspaceDir, markerDirName, "leaf.pid"))
	if err != nil {
		return Info{}, fmt.Errorf("read pid file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		return Info{}, fmt.Errorf("parse pid %q: %w", pidData, err)
	}
	if !pidAlive(pid) {
		return Info{}, ErrLeafUnreachable
	}
	if !healthzOK(cfg.LeafHTTPURL+"/healthz", 500*time.Millisecond) {
		return Info{}, ErrLeafUnreachable
	}
	return Info{
		AgentID:      cfg.AgentID,
		NATSURL:      cfg.NATSURL,
		LeafHTTPURL:  cfg.LeafHTTPURL,
		RepoPath:     cfg.RepoPath,
		WorkspaceDir: workspaceDir,
	}, nil
}
```

Add `"strconv"` and `"strings"` to the `workspace.go` import block.

- [ ] **Step 5: Run tests — expect pass**

Run: `go test ./internal/workspace/ -run TestJoin -v -timeout 60s`
Expected: PASS on all three.

- [ ] **Step 6: Run the full package suite to confirm no regressions**

Run: `go test ./internal/workspace/ -v -timeout 120s`
Expected: PASS on all tests.

- [ ] **Step 7: Commit**

```bash
git add internal/workspace/verify.go internal/workspace/workspace.go internal/workspace/workspace_test.go
git commit -m "feat(workspace): Join reads config + verifies pid and healthz (zh8)"
```

---

## Task 8: CLI main + exit codes + integration test

Wire the binary. The workspace package is done; `main.go` is a thin translator between sentinel errors and exit codes.

**Files:**
- Create: `cmd/agent-init/main.go`
- Create: `cmd/agent-init/integration_test.go`
- Modify: `Makefile`

- [ ] **Step 1: Write the failing integration test**

```go
// cmd/agent-init/integration_test.go
package main_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// binPath points at the built agent-init binary.
// CI and the Makefile are responsible for producing it before running tests.
var binPath = func() string {
	if p := os.Getenv("AGENT_INIT_BIN"); p != "" {
		return p
	}
	return "../../bin/agent-init"
}()

func requireBinaries(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("agent-init binary not built (%v); run `make agent-init`", err)
	}
	if _, err := exec.LookPath(leafBinary()); err != nil {
		t.Skipf("leaf binary not available (%v); set LEAF_BIN", err)
	}
}

func leafBinary() string {
	if p := os.Getenv("LEAF_BIN"); p != "" {
		return p
	}
	return "leaf"
}

func runCmd(t *testing.T, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LEAF_BIN="+leafBinary())
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ProcessState.ExitCode()
		} else {
			t.Fatalf("run: %v", err)
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

func TestCLI_InitAndJoin(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	requireBinaries(t)

	dir := t.TempDir()
	t.Cleanup(func() { killPidFile(t, filepath.Join(dir, ".agent-infra", "leaf.pid")) })

	initOut, initErr, code := runCmd(t, dir, "init")
	if code != 0 {
		t.Fatalf("init exit=%d stdout=%q stderr=%q", code, initOut, initErr)
	}
	if !strings.Contains(initOut, "agent_id=") {
		t.Errorf("init stdout missing agent_id: %q", initOut)
	}

	// Join from the same dir and from a subdir — both should exit 0.
	joinOut, _, code := runCmd(t, dir, "join")
	if code != 0 {
		t.Fatalf("join from root exit=%d: %s", code, joinOut)
	}

	subdir := filepath.Join(dir, "deeper")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	joinSubOut, _, code := runCmd(t, subdir, "join")
	if code != 0 {
		t.Fatalf("join from subdir exit=%d: %s", code, joinSubOut)
	}
}

func TestCLI_InitExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	requireBinaries(t)

	dir := t.TempDir()
	t.Cleanup(func() { killPidFile(t, filepath.Join(dir, ".agent-infra", "leaf.pid")) })

	if _, _, code := runCmd(t, dir, "init"); code != 0 {
		t.Fatalf("first init exit=%d", code)
	}
	_, stderr, code := runCmd(t, dir, "init")
	if code != 2 {
		t.Errorf("second init exit=%d, want 2", code)
	}
	if !strings.Contains(stderr, "already initialized") {
		t.Errorf("stderr missing 'already initialized': %q", stderr)
	}
}

func TestCLI_JoinNoMarker(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	requireBinaries(t)

	dir := t.TempDir()
	_, stderr, code := runCmd(t, dir, "join")
	if code != 3 {
		t.Errorf("exit=%d, want 3", code)
	}
	if !strings.Contains(stderr, "no agent-infra workspace") {
		t.Errorf("stderr missing hint: %q", stderr)
	}
}
```

Import `"fmt"` in the test file.

- [ ] **Step 2: Run test — expect fail**

Run: `go test ./cmd/agent-init/ -run TestCLI -v`
Expected: FAIL (binary not built).

- [ ] **Step 3: Create main.go**

```go
// Command agent-init creates or joins an agent-infra workspace.
//
// Usage:
//
//	agent-init init    # in the directory you want as the workspace root
//	agent-init join    # from cwd or any subdir of an existing workspace
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/danmestas/agent-infra/internal/workspace"
)

const usage = `Usage:
  agent-init init   - create a new workspace in the current directory
  agent-init join   - locate and verify an existing workspace
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-init: cwd: %v\n", err)
		return 1
	}
	// SIGINT/SIGTERM cancels the context. Init and Join are designed to
	// roll back cleanly when their context is canceled mid-flight.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "init":
		info, err := workspace.Init(ctx, cwd)
		return report("init", info, err)
	case "join":
		info, err := workspace.Join(ctx, cwd)
		return report("join", info, err)
	default:
		fmt.Fprintf(os.Stderr, "agent-init: unknown command %q\n%s", args[0], usage)
		return 1
	}
}

func report(op string, info workspace.Info, err error) int {
	if err == nil {
		fmt.Printf("workspace=%s\nagent_id=%s\nnats_url=%s\nleaf_http_url=%s\n",
			info.WorkspaceDir, info.AgentID, info.NATSURL, info.LeafHTTPURL)
		return 0
	}
	switch {
	case errors.Is(err, workspace.ErrAlreadyInitialized):
		fmt.Fprintf(os.Stderr, "agent-init: workspace already initialized; run `agent-init join` instead\n")
		return 2
	case errors.Is(err, workspace.ErrNoWorkspace):
		fmt.Fprintf(os.Stderr, "agent-init: no agent-infra workspace found; run `agent-init init` first\n")
		return 3
	case errors.Is(err, workspace.ErrLeafUnreachable):
		fmt.Fprintf(os.Stderr, "agent-init: leaf daemon not reachable; its PID file may be stale\n")
		return 4
	case errors.Is(err, workspace.ErrLeafStartTimeout):
		fmt.Fprintf(os.Stderr, "agent-init: leaf failed to start within timeout\n")
		return 5
	default:
		fmt.Fprintf(os.Stderr, "agent-init: %s: %v\n", op, err)
		return 1
	}
}
```

- [ ] **Step 4: Add Makefile targets**

Open `Makefile` and add, near existing `build:` target:

```makefile
.PHONY: agent-init
agent-init: bin/
	go build -o bin/agent-init ./cmd/agent-init
```

If the repo has no `bin/` target, add:

```makefile
bin/:
	mkdir -p bin
```

And extend `.PHONY` line at the top to include `agent-init` if it isn't already.

- [ ] **Step 5: Build and run the integration tests**

Run:
```bash
make agent-init
(cd ../EdgeSync && make leaf)
export LEAF_BIN=$(realpath ../EdgeSync/bin/leaf)
go test ./cmd/agent-init/ -v -timeout 120s
```

Expected: PASS on `TestCLI_InitAndJoin`, `TestCLI_InitExitCodes`, `TestCLI_JoinNoMarker`.

- [ ] **Step 6: Commit**

```bash
git add cmd/agent-init/main.go cmd/agent-init/integration_test.go Makefile
git commit -m "feat(agent-init): CLI binary with exit-code contract (zh8)"
```

---

## Task 9: slog + OTel instrumentation

Behavior is correct; add observability.

**Files:**
- Modify: `internal/workspace/workspace.go`
- Modify: `internal/workspace/spawn.go`
- Modify: `internal/workspace/workspace_test.go`
- Modify: `cmd/agent-init/main.go`

- [ ] **Step 1: Write the failing slog test**

Append to `workspace_test.go`:

```go
func TestInit_EmitsSlogEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf")
	}
	requireLeafBinary(t)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := Init(ctx, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { killLeafPID(t, filepath.Join(dir, markerDirName, "leaf.pid")) })

	logs := buf.String()
	for _, want := range []string{`"msg":"init start"`, `"msg":"init complete"`, `"agent_id":"` + info.AgentID + `"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("slog output missing %q.\nFull logs:\n%s", want, logs)
		}
	}
}
```

Add `"bytes"` and `"log/slog"` to test imports.

- [ ] **Step 2: Run — expect fail**

Run: `go test ./internal/workspace/ -run TestInit_EmitsSlogEvents -v -timeout 60s`
Expected: FAIL — slog events missing.

- [ ] **Step 3: Add slog to Init and Join**

In `workspace.go`, modify `Init` to log at start and end. Add `"log/slog"` to imports. At top of `Init`, immediately after determining `markerDir`:

```go
start := time.Now()
slog.InfoContext(ctx, "init start", "cwd", cwd)
defer func() {
	slog.InfoContext(ctx, "init complete",
		"cwd", cwd, "duration_ms", time.Since(start).Milliseconds())
}()
```

Do the same in `Join`:

```go
start := time.Now()
slog.InfoContext(ctx, "join start", "cwd", cwd)
defer func() {
	slog.InfoContext(ctx, "join complete",
		"cwd", cwd, "duration_ms", time.Since(start).Milliseconds())
}()
```

After `cfg` is constructed in `Init`, add:

```go
slog.InfoContext(ctx, "agent_id generated", "agent_id", cfg.AgentID)
```

After loading config in `Join`, add:

```go
slog.InfoContext(ctx, "config loaded", "agent_id", cfg.AgentID)
```

- [ ] **Step 4: Run — expect pass**

Run: `go test ./internal/workspace/ -run TestInit_EmitsSlogEvents -v -timeout 60s`
Expected: PASS.

- [ ] **Step 5: Wire OTel setup in main.go**

In `cmd/agent-init/main.go`, import `github.com/dmestas/edgesync/leaf/telemetry`. Near the top of `run`, after `ctx` is created:

```go
tcfg := telemetry.TelemetryConfig{
	ServiceName: "agent-init",
	Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
}
if hdrs := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"); hdrs != "" {
	tcfg.Headers = parseOTelHeaders(hdrs) // simple "k=v,k=v" splitter helper
}
shutdown, err := telemetry.Setup(ctx, tcfg)
if err != nil {
	fmt.Fprintf(os.Stderr, "agent-init: telemetry setup: %v\n", err)
	// Non-fatal: continue without telemetry.
} else {
	defer func() { _ = shutdown(context.Background()) }()
}
```

Define `parseOTelHeaders` as a small helper in `main.go`:

```go
func parseOTelHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return out
}
```

Add imports to `main.go`: `"github.com/dmestas/edgesync/leaf/telemetry"` and `"strings"`.

Also add slog JSON mode. Near the top of `run`:

```go
if os.Getenv("AGENT_INFRA_LOG") == "json" {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
}
```

Add `"log/slog"` to main.go imports.

- [ ] **Step 6: Add OTel spans around the key Init/Join phases**

In `workspace.go`, add `otelTracerName = "github.com/danmestas/agent-infra/internal/workspace"` and acquire a tracer at package init:

```go
import "go.opentelemetry.io/otel"

var tracer = otel.Tracer("github.com/danmestas/agent-infra/internal/workspace")
```

In `Init`, wrap the body:

```go
ctx, span := tracer.Start(ctx, "agent_init.init")
defer span.End()
// ... existing logic ...
span.SetAttributes(attribute.String("agent_id", cfg.AgentID))
```

Import `"go.opentelemetry.io/otel/attribute"`.

Same treatment in `Join`:

```go
ctx, span := tracer.Start(ctx, "agent_init.join")
defer span.End()
// ...
span.SetAttributes(attribute.String("agent_id", cfg.AgentID))
```

No tests for span emission — the integration test `TestInit_EmitsSlogEvents` already catches regressions in the logging layer; spans are a no-op when no exporter is configured, which is the production default for CI.

- [ ] **Step 7: Add counter + histogram metrics**

In `workspace.go`, acquire a meter alongside the tracer:

```go
import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

var (
	tracer = otel.Tracer("github.com/danmestas/agent-infra/internal/workspace")
	meter  = otel.Meter("github.com/danmestas/agent-infra/internal/workspace")

	opCounter  metric.Int64Counter
	opDuration metric.Float64Histogram
)

func init() {
	var err error
	opCounter, err = meter.Int64Counter("agent_init.operations.total")
	if err != nil {
		panic(err)
	}
	opDuration, err = meter.Float64Histogram("agent_init.operation.duration.seconds")
	if err != nil {
		panic(err)
	}
}
```

Then in `Init`, at the `defer` that logs completion, also record metrics. Replace:

```go
defer func() {
	slog.InfoContext(ctx, "init complete",
		"cwd", cwd, "duration_ms", time.Since(start).Milliseconds())
}()
```

with:

```go
defer func(err *error) {
	result := "success"
	if *err != nil {
		result = "error"
	}
	opCounter.Add(ctx, 1,
		metric.WithAttributes(attribute.String("op", "init"), attribute.String("result", result)))
	opDuration.Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(attribute.String("op", "init")))
	slog.InfoContext(ctx, "init complete",
		"cwd", cwd, "duration_ms", time.Since(start).Milliseconds(), "result", result)
}(&err)
```

Requires naming the error return (change `func Init(ctx context.Context, cwd string) (Info, error)` to `func Init(ctx context.Context, cwd string) (info Info, err error)`). Same treatment for `Join`.

- [ ] **Step 8: Run full suites**

Run:
```bash
go mod tidy
go test ./internal/workspace/ -v -timeout 120s
go test ./cmd/agent-init/ -v -timeout 120s
go vet ./...
```
Expected: all PASS; no vet issues.

- [ ] **Step 9: Commit**

```bash
git add .
git commit -m "feat(workspace): slog + OTel spans + metrics on Init/Join (zh8)"
```

---

## Task 10: CI + Makefile polish

Make sure the new suite runs in CI without disrupting existing tests.

**Files:**
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1: Inspect the existing CI workflow**

Run: `cat .github/workflows/ci.yml`

Confirm how current tests are invoked (likely via `make check` or `go test ./...`).

- [ ] **Step 2: Ensure CI builds leaf before integration tests**

If `make check` already runs `go test ./...`, the integration tests will pick up automatically. But they need `leaf` on PATH to not skip.

In `.github/workflows/ci.yml`, add a step before `make check`:

```yaml
      - name: Build leaf binary (for integration tests)
        run: |
          cd ../EdgeSync
          make leaf
          echo "LEAF_BIN=$GITHUB_WORKSPACE/../EdgeSync/bin/leaf" >> $GITHUB_ENV
```

(Path may need adjusting to wherever EdgeSync is checked out in CI. Reuse the path logic from the existing EdgeSync checkout step.)

- [ ] **Step 3: Update Makefile check target**

Verify that `make check` (or equivalent) invokes `go test ./...` with a reasonable timeout, and that the integration tests are included. If the Makefile has a `build` target, add `agent-init` to its dependencies:

```makefile
build: bin/ leaf-build agent-init
```

- [ ] **Step 4: Run `make check` locally**

Run: `make check`
Expected: PASS (or whatever the existing success output is). No new failures.

- [ ] **Step 5: Commit**

```bash
git add Makefile .github/workflows/ci.yml
git commit -m "ci: build agent-init and leaf, run integration suite (zh8)"
```

---

## Task 11: Update docs + close ticket

Close the loop.

**Files:**
- Modify: `README.md` (or create `docs/agent-init.md`)
- bd: close `agent-infra-zh8`

- [ ] **Step 1: Document agent-init in README**

Add a "Getting started" subsection that shows:

```bash
# First time in a fresh directory:
$ agent-init init
workspace=/path/to/dir
agent_id=...
nats_url=nats://127.0.0.1:4222
leaf_http_url=http://127.0.0.1:51234

# From anywhere below the workspace:
$ agent-init join
workspace=/path/to/dir
...
```

And note: the `leaf` binary must be on PATH (or `$LEAF_BIN` set). Logs land in `.agent-infra/leaf.log`.

- [ ] **Step 2: Commit docs**

```bash
git add README.md
git commit -m "docs: agent-init getting-started section (zh8)"
```

- [ ] **Step 3: Close the ticket**

```bash
bd close agent-infra-zh8 --reason="cmd/agent-init shipped with workspace Init/Join, real-leaf integration tests, exit-code contract, slog + OTel"
git add .beads/issues.jsonl
git commit -m "bd: close zh8"
```

- [ ] **Step 4: Push**

```bash
git pull --rebase
git push
git status  # should show: up to date with origin
```
