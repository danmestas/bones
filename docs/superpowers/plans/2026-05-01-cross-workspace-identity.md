# Cross-Workspace Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement filesystem registry of running bones workspaces, session marker tracking, `bones status --all`, `bones down --all`, `BONES_WORKSPACE` shell integration, and `bones rename` per the design spec.

**Architecture:** Two new packages — `internal/registry/` (workspace entries) and `internal/sessions/` (session markers) — provide read/write/list/prune primitives with one JSON file per entity. Existing `bones up`/`down`/`status` are extended with registry I/O and `--all` flags. Three new CLI verbs (`bones env`, `bones rename`, hidden `bones session-marker`) provide shell-prompt integration and rename UX.

**Tech Stack:** Go 1.23+, Kong CLI framework, `crypto/sha256` + `encoding/hex` for workspace IDs, `os` + `path/filepath` for atomic writes, standard `testing` package, `slog` for logging.

**Spec:** `docs/superpowers/specs/2026-05-01-cross-workspace-identity-design.md`

---

## File structure

```
cli/
  env.go                    # NEW — bones env verb
  env_test.go               # NEW
  rename.go                 # NEW — bones rename verb
  rename_test.go            # NEW
  session_marker.go         # NEW — hidden bones session-marker subcommand
  session_marker_test.go    # NEW
  status.go                 # MODIFY — add --all flag
  status_test.go            # MODIFY — add --all tests
  down.go                   # MODIFY — add --all flag, registry remove
  down_test.go              # MODIFY — add --all tests
  up.go                     # MODIFY — registry write at end of bootstrap

cmd/bones/
  cli.go                    # MODIFY — register Env, Rename, SessionMarker

internal/
  registry/
    registry.go             # NEW — workspace registry primitives
    registry_test.go        # NEW
    health.go               # NEW — IsAlive (PID + HTTP) for stale detection
    health_test.go          # NEW
  sessions/
    sessions.go             # NEW — session marker primitives
    sessions_test.go        # NEW
  workspace/
    workspace.go            # MODIFY — add WorkspaceName field + read/write helpers
    workspace_test.go       # MODIFY — tests for new field

internal/hub/
  hub.go                    # MODIFY — ensure /health endpoint exists (no-op if already present)

docs/adr/
  0038-per-workspace-hub-ports.md   # MODIFY — add Future-supersession-trigger footnote

README.md                   # MODIFY — add shell prompt-hook + theme snippets
```

---

## Phase 1: Registry foundation (Tasks 1–7)

### Task 1: Workspace ID derivation

**Files:**
- Create: `internal/registry/registry.go`
- Test: `internal/registry/registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/registry/registry_test.go
package registry

import "testing"

func TestWorkspaceID(t *testing.T) {
	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"simple path", "/Users/dan/projects/foo", "5a7c0b2c4d3e9f8a"},
		{"trailing slash normalized", "/Users/dan/projects/foo/", "5a7c0b2c4d3e9f8a"},
		{"different path", "/Users/dan/projects/bar", "8f3a1b9c7d6e2a4b"},
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
```

Note: hardcoded expected hex values (`5a7c0b2c...`) are illustrative; replace with actual SHA256 prefix during step 4 once you compute them. The structural assertions (length 16, determinism, no collision) carry the test.

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/registry/ -run TestWorkspaceID
```
Expected: FAIL with `undefined: WorkspaceID` or similar.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/registry/registry.go
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// WorkspaceID returns a deterministic 16-hex-char identifier for an absolute cwd.
// Used as the registry filename: ~/.bones/workspaces/<id>.json
func WorkspaceID(cwd string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return hex.EncodeToString(sum[:])[:16]
}
```

- [ ] **Step 4: Run test to verify it passes**

Compute actual SHA256 prefixes for the test fixtures and update the `want` values in the test, then:
```bash
go test ./internal/registry/ -run TestWorkspaceID -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry/registry.go internal/registry/registry_test.go
git commit -m "feat(registry): add WorkspaceID derivation (sha256 prefix)"
```

---

### Task 2: Registry entry struct + JSON marshaling

**Files:**
- Modify: `internal/registry/registry.go`
- Test: `internal/registry/registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
// Append to internal/registry/registry_test.go
import (
	"encoding/json"
	"testing"
	"time"
)

func TestEntryJSON(t *testing.T) {
	e := Entry{
		Cwd:       "/Users/dan/projects/foo",
		Name:      "foo",
		HubURL:    "http://127.0.0.1:8765",
		NATSURL:   "nats://127.0.0.1:4222",
		HubPID:    12345,
		StartedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(e)
	if err != nil { t.Fatalf("Marshal: %v", err) }

	var got Entry
	if err := json.Unmarshal(data, &got); err != nil { t.Fatalf("Unmarshal: %v", err) }
	if got != e { t.Fatalf("round-trip: got %+v, want %+v", got, e) }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/registry/ -run TestEntryJSON
```
Expected: FAIL with `undefined: Entry`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/registry/registry.go`:

```go
import "time"

// Entry is one workspace's registry record. One JSON file per Entry at
// ~/.bones/workspaces/<WorkspaceID>.json.
type Entry struct {
	Cwd       string    `json:"cwd"`
	Name      string    `json:"name"`
	HubURL    string    `json:"hub_url"`
	NATSURL   string    `json:"nats_url"`
	HubPID    int       `json:"hub_pid"`
	StartedAt time.Time `json:"started_at"`
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/registry/ -run TestEntryJSON -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry/registry.go internal/registry/registry_test.go
git commit -m "feat(registry): add Entry struct + JSON round-trip"
```

---

### Task 3: Atomic registry write

**Files:**
- Modify: `internal/registry/registry.go`
- Test: `internal/registry/registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // RegistryDir derives from $HOME
	e := Entry{
		Cwd: "/Users/dan/projects/foo", Name: "foo",
		HubURL: "http://127.0.0.1:8765", HubPID: 12345,
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := Write(e); err != nil { t.Fatalf("Write: %v", err) }

	// File should exist at expected path
	path := filepath.Join(dir, ".bones", "workspaces", WorkspaceID(e.Cwd)+".json")
	if _, err := os.Stat(path); err != nil { t.Fatalf("expected file at %s: %v", path, err) }

	// No tmp file leftover
	matches, _ := filepath.Glob(filepath.Join(dir, ".bones", "workspaces", "*.tmp.*"))
	if len(matches) > 0 { t.Fatalf("tmp file leaked: %v", matches) }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/registry/ -run TestWrite
```
Expected: FAIL with `undefined: Write`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/registry/registry.go`:

```go
import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// RegistryDir returns the directory that holds workspace entry files.
// Always rooted at $HOME/.bones/workspaces/.
func RegistryDir() string {
	return filepath.Join(os.Getenv("HOME"), ".bones", "workspaces")
}

// EntryPath returns the absolute path of the JSON file for the given workspace cwd.
func EntryPath(cwd string) string {
	return filepath.Join(RegistryDir(), WorkspaceID(cwd)+".json")
}

// Write persists e to its file atomically (tmp+rename). Creates the registry
// directory if missing.
func Write(e Entry) error {
	if err := os.MkdirAll(RegistryDir(), 0o755); err != nil {
		return fmt.Errorf("registry mkdir: %w", err)
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil { return fmt.Errorf("registry marshal: %w", err) }

	dst := EntryPath(e.Cwd)
	tmp, err := os.CreateTemp(RegistryDir(), filepath.Base(dst)+".tmp.*")
	if err != nil { return fmt.Errorf("registry tmp: %w", err) }
	defer os.Remove(tmp.Name()) // safe; rename succeeded means tmp is gone

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.Join(fmt.Errorf("registry write: %w", err), tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("registry sync: %w", err)
	}
	if err := tmp.Close(); err != nil { return fmt.Errorf("registry close: %w", err) }
	return os.Rename(tmp.Name(), dst)
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/registry/ -run TestWrite -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry/registry.go internal/registry/registry_test.go
git commit -m "feat(registry): add atomic Write (tmp+rename)"
```

---

### Task 4: Registry Read

**Files:** `internal/registry/registry.go`, `internal/registry/registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	want := Entry{Cwd: "/x", Name: "x", HubURL: "u", HubPID: 1, StartedAt: time.Now().UTC().Truncate(time.Second)}
	if err := Write(want); err != nil { t.Fatalf("Write: %v", err) }

	got, err := Read(want.Cwd)
	if err != nil { t.Fatalf("Read: %v", err) }
	if got != want { t.Fatalf("Read mismatch: got %+v, want %+v", got, want) }

	// Missing entry returns ErrNotFound
	if _, err := Read("/nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/registry/ -run TestRead
```
Expected: FAIL `undefined: Read` and `undefined: ErrNotFound`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/registry/registry.go`:

```go
// ErrNotFound is returned by Read when no entry exists for the given cwd.
var ErrNotFound = errors.New("registry: entry not found")

// Read returns the Entry for a workspace, or ErrNotFound if no entry exists.
func Read(cwd string) (Entry, error) {
	data, err := os.ReadFile(EntryPath(cwd))
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, ErrNotFound
	}
	if err != nil { return Entry{}, fmt.Errorf("registry read: %w", err) }
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, fmt.Errorf("registry unmarshal: %w", err)
	}
	return e, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/registry/ -run TestRead -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry/registry.go internal/registry/registry_test.go
git commit -m "feat(registry): add Read with ErrNotFound"
```

---

### Task 5: Registry List

**Files:** `internal/registry/registry.go`, `internal/registry/registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestList(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	entries := []Entry{
		{Cwd: "/a", Name: "a", HubPID: 1, StartedAt: time.Now().UTC().Truncate(time.Second)},
		{Cwd: "/b", Name: "b", HubPID: 2, StartedAt: time.Now().UTC().Truncate(time.Second)},
	}
	for _, e := range entries {
		if err := Write(e); err != nil { t.Fatalf("Write: %v", err) }
	}

	got, err := List()
	if err != nil { t.Fatalf("List: %v", err) }
	if len(got) != 2 { t.Fatalf("List len = %d, want 2", len(got)) }

	// Empty registry returns empty slice, no error
	t.Setenv("HOME", t.TempDir())
	got, err = List()
	if err != nil { t.Fatalf("List on empty: %v", err) }
	if len(got) != 0 { t.Fatalf("expected empty slice, got %v", got) }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/registry/ -run TestList
```
Expected: FAIL `undefined: List`.

- [ ] **Step 3: Write minimal implementation**

```go
// List returns all registry entries on this user/host. Returns an empty slice
// (not an error) if the registry directory doesn't exist.
func List() ([]Entry, error) {
	matches, err := filepath.Glob(filepath.Join(RegistryDir(), "*.json"))
	if err != nil { return nil, fmt.Errorf("registry glob: %w", err) }
	out := make([]Entry, 0, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil { continue } // skip unreadable files (concurrent removal)
		var e Entry
		if err := json.Unmarshal(data, &e); err != nil { continue } // skip corrupt
		out = append(out, e)
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/registry/ -run TestList -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry/registry.go internal/registry/registry_test.go
git commit -m "feat(registry): add List (glob + parse, skip corrupt)"
```

---

### Task 6: Registry Remove

**Files:** `internal/registry/registry.go`, `internal/registry/registry_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRemove(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	e := Entry{Cwd: "/x", Name: "x", HubPID: 1, StartedAt: time.Now().UTC().Truncate(time.Second)}
	if err := Write(e); err != nil { t.Fatalf("Write: %v", err) }

	if err := Remove(e.Cwd); err != nil { t.Fatalf("Remove: %v", err) }
	if _, err := Read(e.Cwd); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Remove, got %v", err)
	}

	// Remove of nonexistent entry is a no-op (idempotent)
	if err := Remove("/never-existed"); err != nil {
		t.Fatalf("Remove nonexistent: want nil, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/registry/ -run TestRemove
```
Expected: FAIL `undefined: Remove`.

- [ ] **Step 3: Write minimal implementation**

```go
// Remove deletes the registry entry for cwd. Idempotent: missing entry returns nil.
func Remove(cwd string) error {
	err := os.Remove(EntryPath(cwd))
	if err == nil || errors.Is(err, os.ErrNotExist) { return nil }
	return fmt.Errorf("registry remove: %w", err)
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/registry/ -run TestRemove -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry/registry.go internal/registry/registry_test.go
git commit -m "feat(registry): add idempotent Remove"
```

---

### Task 7: Stale detection (PID + HTTP /health)

**Files:**
- Create: `internal/registry/health.go`
- Test: `internal/registry/health_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/registry/health_test.go
package registry

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestIsAlive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" { w.WriteHeader(200); return }
		w.WriteHeader(404)
	}))
	defer srv.Close()

	t.Run("alive: PID + healthy HTTP", func(t *testing.T) {
		e := Entry{HubPID: os.Getpid(), HubURL: srv.URL}
		if !IsAlive(e) { t.Fatalf("expected alive") }
	})

	t.Run("dead: PID alive but HTTP wrong port", func(t *testing.T) {
		e := Entry{HubPID: os.Getpid(), HubURL: "http://127.0.0.1:1"} // unbound
		if IsAlive(e) { t.Fatalf("expected dead (HTTP fails)") }
	})

	t.Run("dead: PID gone", func(t *testing.T) {
		e := Entry{HubPID: 0, HubURL: srv.URL} // PID 0 always invalid
		if IsAlive(e) { t.Fatalf("expected dead (PID invalid)") }
	})
}

// Sanity check: server URL contains 127.0.0.1 + port
func TestServerURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1:") {
		t.Fatalf("unexpected URL: %s", srv.URL)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/registry/ -run TestIsAlive
```
Expected: FAIL `undefined: IsAlive`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/registry/health.go
package registry

import (
	"net/http"
	"os"
	"syscall"
	"time"
)

// HealthTimeout bounds the HTTP /health probe. Tunable via env BONES_REGISTRY_PROBE_TIMEOUT.
var HealthTimeout = 500 * time.Millisecond

// IsAlive returns true if BOTH (a) the recorded HubPID is alive on this host
// AND (b) GET <HubURL>/health returns 200 within HealthTimeout. Both checks are
// required because a recycled PID can pass (a) but fail (b).
func IsAlive(e Entry) bool {
	if !pidAlive(e.HubPID) { return false }
	client := &http.Client{Timeout: HealthTimeout}
	resp, err := client.Get(e.HubURL + "/health")
	if err != nil { return false }
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func pidAlive(pid int) bool {
	if pid <= 0 { return false }
	proc, err := os.FindProcess(pid)
	if err != nil { return false }
	// Signal 0 is a no-op probe on POSIX
	return proc.Signal(syscall.Signal(0)) == nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/registry/ -run TestIsAlive -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/registry/health.go internal/registry/health_test.go
git commit -m "feat(registry): add IsAlive (PID + HTTP /health probe)"
```

---

## Phase 2: Session markers (Tasks 8–10)

### Task 8: Session marker struct + Write

**Files:**
- Create: `internal/sessions/sessions.go`
- Test: `internal/sessions/sessions_test.go`

- [ ] **Step 1: Write the failing test**

```go
// internal/sessions/sessions_test.go
package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMarkerJSON(t *testing.T) {
	m := Marker{
		SessionID:    "ade241a5-b8c7-4d3f-9e2a-1b6c8d7f5a3e",
		WorkspaceCwd: "/Users/dan/projects/foo",
		ClaudePID:    67890,
		StartedAt:    time.Now().UTC().Truncate(time.Second),
	}
	data, _ := json.Marshal(m)
	var got Marker
	if err := json.Unmarshal(data, &got); err != nil { t.Fatalf("Unmarshal: %v", err) }
	if got != m { t.Fatalf("round-trip: got %+v, want %+v", got, m) }
}

func TestRegister(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	m := Marker{SessionID: "abc", WorkspaceCwd: "/x", ClaudePID: os.Getpid(), StartedAt: time.Now().UTC()}
	if err := Register(m); err != nil { t.Fatalf("Register: %v", err) }

	path := filepath.Join(dir, ".bones", "sessions", m.SessionID+".json")
	if _, err := os.Stat(path); err != nil { t.Fatalf("expected file at %s: %v", path, err) }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/sessions/ -run TestMarker -run TestRegister
```
Expected: FAIL `undefined: Marker, Register`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/sessions/sessions.go
package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Marker is a per-session record. One JSON file per Marker at
// ~/.bones/sessions/<SessionID>.json.
type Marker struct {
	SessionID    string    `json:"session_id"`
	WorkspaceCwd string    `json:"workspace_cwd"`
	ClaudePID    int       `json:"claude_pid"`
	StartedAt    time.Time `json:"started_at"`
}

// SessionsDir returns the directory holding session marker files.
func SessionsDir() string {
	return filepath.Join(os.Getenv("HOME"), ".bones", "sessions")
}

// MarkerPath returns the file path for a given session ID.
func MarkerPath(sessionID string) string {
	return filepath.Join(SessionsDir(), sessionID+".json")
}

// Register persists m atomically (tmp+rename).
func Register(m Marker) error {
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		return fmt.Errorf("sessions mkdir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil { return fmt.Errorf("marker marshal: %w", err) }

	dst := MarkerPath(m.SessionID)
	tmp, err := os.CreateTemp(SessionsDir(), filepath.Base(dst)+".tmp.*")
	if err != nil { return fmt.Errorf("marker tmp: %w", err) }
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil { tmp.Close(); return err }
	if err := tmp.Sync(); err != nil { tmp.Close(); return err }
	if err := tmp.Close(); err != nil { return err }
	return os.Rename(tmp.Name(), dst)
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/sessions/ -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sessions/sessions.go internal/sessions/sessions_test.go
git commit -m "feat(sessions): add Marker struct + atomic Register"
```

---

### Task 9: Session Unregister + ListByWorkspace

**Files:** `internal/sessions/sessions.go`, `internal/sessions/sessions_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestUnregister(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := Marker{SessionID: "abc", WorkspaceCwd: "/x", ClaudePID: os.Getpid(), StartedAt: time.Now().UTC()}
	if err := Register(m); err != nil { t.Fatalf("Register: %v", err) }
	if err := Unregister(m.SessionID); err != nil { t.Fatalf("Unregister: %v", err) }
	if _, err := os.Stat(MarkerPath(m.SessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still exists after Unregister")
	}
	// Idempotent
	if err := Unregister("never-existed"); err != nil {
		t.Fatalf("Unregister nonexistent: %v", err)
	}
}

func TestListByWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	myPID := os.Getpid()
	now := time.Now().UTC()
	for _, m := range []Marker{
		{SessionID: "s1", WorkspaceCwd: "/a", ClaudePID: myPID, StartedAt: now},
		{SessionID: "s2", WorkspaceCwd: "/a", ClaudePID: myPID, StartedAt: now},
		{SessionID: "s3", WorkspaceCwd: "/b", ClaudePID: myPID, StartedAt: now},
		{SessionID: "s4-dead", WorkspaceCwd: "/a", ClaudePID: 0, StartedAt: now}, // PID 0 = invalid
	} {
		Register(m)
	}

	got := ListByWorkspace("/a")
	if len(got) != 2 { // s4-dead is filtered by PID-alive check
		t.Fatalf("expected 2 alive markers for /a, got %d", len(got))
	}

	if g := ListByWorkspace("/b"); len(g) != 1 {
		t.Fatalf("expected 1 marker for /b, got %d", len(g))
	}

	if g := ListByWorkspace("/none"); len(g) != 0 {
		t.Fatalf("expected 0 markers for /none, got %d", len(g))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/sessions/
```
Expected: FAIL `undefined: Unregister, ListByWorkspace`.

- [ ] **Step 3: Write minimal implementation**

```go
import "syscall"

// Unregister deletes the marker file. Idempotent.
func Unregister(sessionID string) error {
	err := os.Remove(MarkerPath(sessionID))
	if err == nil || errors.Is(err, os.ErrNotExist) { return nil }
	return fmt.Errorf("marker remove: %w", err)
}

// ListByWorkspace returns markers whose WorkspaceCwd matches the argument
// AND whose ClaudePID is alive on this host. Dead markers are unlinked as a
// side effect (orphan GC).
func ListByWorkspace(cwd string) []Marker {
	matches, _ := filepath.Glob(filepath.Join(SessionsDir(), "*.json"))
	out := make([]Marker, 0, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil { continue }
		var m Marker
		if err := json.Unmarshal(data, &m); err != nil { continue }
		if !pidAlive(m.ClaudePID) {
			os.Remove(path) // GC dead marker
			continue
		}
		if m.WorkspaceCwd == cwd { out = append(out, m) }
	}
	return out
}

func pidAlive(pid int) bool {
	if pid <= 0 { return false }
	p, err := os.FindProcess(pid)
	if err != nil { return false }
	return p.Signal(syscall.Signal(0)) == nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/sessions/ -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sessions/sessions.go internal/sessions/sessions_test.go
git commit -m "feat(sessions): add Unregister + ListByWorkspace with PID-alive GC"
```

---

### Task 10: CountByWorkspace (convenience)

**Files:** `internal/sessions/sessions.go`, `internal/sessions/sessions_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestCountByWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	myPID := os.Getpid()
	for i, cwd := range []string{"/a", "/a", "/b"} {
		Register(Marker{SessionID: fmt.Sprintf("s%d", i), WorkspaceCwd: cwd, ClaudePID: myPID, StartedAt: time.Now().UTC()})
	}
	if got := CountByWorkspace("/a"); got != 2 {
		t.Fatalf("CountByWorkspace(/a) = %d, want 2", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/sessions/ -run TestCountByWorkspace
```
Expected: FAIL `undefined: CountByWorkspace`.

- [ ] **Step 3: Write minimal implementation**

```go
// CountByWorkspace returns the number of alive session markers attached to cwd.
func CountByWorkspace(cwd string) int { return len(ListByWorkspace(cwd)) }
```

Add `import "fmt"` to test file if not already present.

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/sessions/ -run TestCountByWorkspace -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sessions/sessions.go internal/sessions/sessions_test.go
git commit -m "feat(sessions): add CountByWorkspace convenience"
```

---

## Phase 3: Hidden `bones session-marker` subcommand (Tasks 11–12)

### Task 11: SessionMarkerCmd struct + Register subcommand

**Files:**
- Create: `cli/session_marker.go`
- Test: `cli/session_marker_test.go`
- Modify: `cmd/bones/cli.go`

- [ ] **Step 1: Write the failing test**

```go
// cli/session_marker_test.go
package cli

import (
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
	if err := cmd.Run(); err != nil { t.Fatalf("Run: %v", err) }

	// Marker file should exist
	path := filepath.Join(os.Getenv("HOME"), ".bones", "sessions", cmd.SessionID+".json")
	if _, err := os.Stat(path); err != nil { t.Fatalf("expected marker at %s: %v", path, err) }

	// Verify ListByWorkspace finds it
	if got := sessions.ListByWorkspace(cmd.Cwd); len(got) != 1 {
		t.Fatalf("ListByWorkspace = %d markers, want 1", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./cli/ -run TestSessionMarkerRegister
```
Expected: FAIL `undefined: SessionMarkerRegisterCmd`.

- [ ] **Step 3: Write minimal implementation**

```go
// cli/session_marker.go
package cli

import (
	"fmt"
	"time"

	"github.com/danmestas/bones/internal/sessions"
)

// SessionMarkerCmd is hidden from --help; it exists only for bones-managed
// SessionStart/End hooks to call. Schema for marker files lives in the
// sessions package; this verb is the only call site.
type SessionMarkerCmd struct {
	Register   SessionMarkerRegisterCmd   `cmd:"" name:"register"`
	Unregister SessionMarkerUnregisterCmd `cmd:"" name:"unregister"`
}

type SessionMarkerRegisterCmd struct {
	SessionID string `name:"session-id" required:""`
	Cwd       string `name:"cwd" required:"" help:"absolute workspace cwd"`
	PID       int    `name:"pid" required:"" help:"claude (or harness) process PID"`
}

func (c *SessionMarkerRegisterCmd) Run() error {
	return sessions.Register(sessions.Marker{
		SessionID:    c.SessionID,
		WorkspaceCwd: c.Cwd,
		ClaudePID:    c.PID,
		StartedAt:    time.Now().UTC(),
	})
}

type SessionMarkerUnregisterCmd struct {
	SessionID string `name:"session-id" required:""`
}

func (c *SessionMarkerUnregisterCmd) Run() error {
	if c.SessionID == "" { return fmt.Errorf("--session-id required") }
	return sessions.Unregister(c.SessionID)
}
```

Modify `cmd/bones/cli.go` to register the verb. Find the CLI struct and add a field:

```go
SessionMarker bonescli.SessionMarkerCmd `cmd:"" name:"session-marker" hidden:"" help:"internal: hook-managed session markers"`
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./cli/ -run TestSessionMarkerRegister -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/session_marker.go cli/session_marker_test.go cmd/bones/cli.go
git commit -m "feat(cli): add hidden bones session-marker register|unregister"
```

---

### Task 12: SessionMarker Unregister test

**Files:** `cli/session_marker_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSessionMarkerUnregister(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg := SessionMarkerRegisterCmd{SessionID: "to-remove", Cwd: "/x", PID: os.Getpid()}
	if err := reg.Run(); err != nil { t.Fatalf("Register: %v", err) }

	un := SessionMarkerUnregisterCmd{SessionID: "to-remove"}
	if err := un.Run(); err != nil { t.Fatalf("Unregister: %v", err) }

	if _, err := os.Stat(sessions.MarkerPath("to-remove")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker still exists after Unregister")
	}
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cli/ -run TestSessionMarkerUnregister -v
```
Expected: PASS (already implemented in Task 11; this test verifies it works end-to-end).

- [ ] **Step 3-5 collapsed: Commit**

If the test passes immediately, commit (no implementation change needed):

```bash
git add cli/session_marker_test.go
git commit -m "test(cli): cover session-marker unregister round-trip"
```

---

## Phase 4: bones up/down registry hooks (Tasks 13–14)

### Task 13: bones up writes registry entry

**Files:** `cli/up.go`, `cli/up_test.go` (new or existing)

- [ ] **Step 1: Write the failing test**

```go
// cli/up_test.go (add to existing file or create)
import (
	"os"
	"path/filepath"
	"testing"
	"github.com/danmestas/bones/internal/registry"
)

func TestUpWritesRegistry(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wsDir := t.TempDir()

	if err := runUp(wsDir, false); err != nil {
		t.Fatalf("runUp: %v", err)
	}

	got, err := registry.Read(wsDir)
	if err != nil { t.Fatalf("registry.Read after up: %v", err) }
	if got.Cwd != wsDir { t.Fatalf("registry Cwd = %q, want %q", got.Cwd, wsDir) }
	if got.HubPID == 0 { t.Fatalf("registry HubPID = 0 (expected nonzero)") }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./cli/ -run TestUpWritesRegistry
```
Expected: FAIL — registry entry not found.

- [ ] **Step 3: Modify `runUp()` in `cli/up.go`** to write the registry entry after successful bootstrap. Find the success path (around line 69 per Explore report) and add:

```go
// At the end of runUp() before returning nil:
import (
	"os"
	"time"
	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/workspace"
)

// ... existing bootstrap completes ...

info, _ := workspace.Join(cwd) // get NATS/HTTP URLs for the registry
if err := registry.Write(registry.Entry{
	Cwd:       cwd,
	Name:      filepath.Base(cwd),
	HubURL:    info.LeafHTTPURL,
	NATSURL:   info.NATSURL,
	HubPID:    os.Getpid(),
	StartedAt: time.Now().UTC(),
}); err != nil {
	// Non-fatal: hub still works locally without registry
	slog.Warn("registry write failed (non-fatal)", "err", err)
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./cli/ -run TestUpWritesRegistry -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/up.go cli/up_test.go
git commit -m "feat(cli): bones up writes workspace registry entry"
```

---

### Task 14: bones down removes registry entry

**Files:** `cli/down.go`, `cli/down_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestDownRemovesRegistry(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wsDir := t.TempDir()

	// Seed registry
	registry.Write(registry.Entry{Cwd: wsDir, Name: "test", HubPID: os.Getpid(), StartedAt: time.Now().UTC()})

	cmd := DownCmd{Yes: true, KeepHub: true} // KeepHub avoids actually killing hub
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil {
		t.Fatalf("Down: %v", err)
	}

	if _, err := registry.Read(wsDir); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("expected registry entry removed, got %v", err)
	}
}
```

Note: `libfossilcli` is the existing import path; verify against current `cli/down.go` imports.

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./cli/ -run TestDownRemovesRegistry
```
Expected: FAIL — entry still present.

- [ ] **Step 3: Modify `runDown()` in `cli/down.go`** to add registry removal. Find the action list around lines 42–50. Add a step:

```go
// In planDown() or runDown(), append:
import "github.com/danmestas/bones/internal/registry"

// As a last step in the down sequence (before returning success):
if err := registry.Remove(root); err != nil {
	slog.Warn("registry remove failed (non-fatal)", "err", err)
}
```

- [ ] **Step 4: Run test**

```bash
go test ./cli/ -run TestDownRemovesRegistry -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/down.go cli/down_test.go
git commit -m "feat(cli): bones down removes registry entry"
```

---

## Phase 5: bones status --all (Tasks 15–16)

### Task 15: Add --all flag to StatusCmd, render multi-workspace table

**Files:** `cli/status.go`, `cli/status_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestStatusAllRendersTable(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Seed two registry entries
	now := time.Now().UTC()
	registry.Write(registry.Entry{Cwd: "/Users/dan/foo", Name: "foo", HubURL: "http://127.0.0.1:1", NATSURL: "nats://x", HubPID: os.Getpid(), StartedAt: now})
	registry.Write(registry.Entry{Cwd: "/Users/dan/bar", Name: "bar", HubURL: "http://127.0.0.1:2", NATSURL: "nats://x", HubPID: os.Getpid(), StartedAt: now})

	var buf bytes.Buffer
	if err := renderStatusAll(&buf); err != nil {
		t.Fatalf("renderStatusAll: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"WORKSPACE", "foo", "bar", "PATH"} {
		if !strings.Contains(out, want) { t.Fatalf("output missing %q:\n%s", want, out) }
	}
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cli/ -run TestStatusAllRendersTable
```
Expected: FAIL `undefined: renderStatusAll`.

- [ ] **Step 3: Write implementation**

Modify `cli/status.go`:

```go
type StatusCmd struct {
	All  bool `name:"all" help:"show status across all workspaces on this user/host"`
	JSON bool `name:"json" help:"emit machine-readable JSON"`
}

func (c *StatusCmd) Run(g *libfossilcli.Globals) error {
	if c.All {
		if c.JSON { return renderStatusAllJSON(os.Stdout) }
		return renderStatusAll(os.Stdout)
	}
	// ... existing single-workspace path unchanged ...
}

// renderStatusAll iterates the registry, prunes stale entries, and prints a table.
func renderStatusAll(w io.Writer) error {
	entries, err := registry.List()
	if err != nil { return err }

	// Prune stale entries (live-only semantics)
	live := entries[:0]
	for _, e := range entries {
		if registry.IsAlive(e) {
			live = append(live, e)
		} else {
			registry.Remove(e.Cwd)
		}
	}

	if len(live) == 0 {
		fmt.Fprintln(w, "No workspaces running. Use 'bones up' in a project.")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKSPACE\tPATH\tHUB\tSESSIONS\tUPTIME")
	for _, e := range live {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			e.Name,
			shortenHome(e.Cwd),
			extractPort(e.HubURL),
			sessions.CountByWorkspace(e.Cwd),
			humanDuration(time.Since(e.StartedAt)),
		)
	}
	return tw.Flush()
}

func shortenHome(p string) string {
	if home := os.Getenv("HOME"); home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

func extractPort(url string) string {
	// Returns ":8765" from "http://127.0.0.1:8765"
	idx := strings.LastIndex(url, ":")
	if idx < 0 { return url }
	return url[idx:]
}

func humanDuration(d time.Duration) string {
	if d < time.Minute { return fmt.Sprintf("%ds", int(d.Seconds())) }
	if d < time.Hour { return fmt.Sprintf("%dm", int(d.Minutes())) }
	return fmt.Sprintf("%dh", int(d.Hours()))
}
```

Add imports: `bytes`, `io`, `strings`, `text/tabwriter`, `github.com/danmestas/bones/internal/registry`, `github.com/danmestas/bones/internal/sessions`.

- [ ] **Step 4: Run test**

```bash
go test ./cli/ -run TestStatusAllRendersTable -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/status.go cli/status_test.go
git commit -m "feat(cli): bones status --all renders cross-workspace table"
```

---

### Task 16: bones status --all --json output

**Files:** `cli/status.go`, `cli/status_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestStatusAllJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	registry.Write(registry.Entry{Cwd: "/x", Name: "x", HubURL: "http://127.0.0.1:1", HubPID: os.Getpid(), StartedAt: time.Now().UTC()})

	var buf bytes.Buffer
	if err := renderStatusAllJSON(&buf); err != nil { t.Fatalf("renderStatusAllJSON: %v", err) }

	var got struct {
		Workspaces []struct{ Name, Cwd string } `json:"workspaces"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil { t.Fatalf("unmarshal: %v", err) }
	if len(got.Workspaces) != 1 { t.Fatalf("workspaces len = %d, want 1", len(got.Workspaces)) }
	if got.Workspaces[0].Name != "x" { t.Fatalf("name = %q, want x", got.Workspaces[0].Name) }
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cli/ -run TestStatusAllJSON
```
Expected: FAIL `undefined: renderStatusAllJSON`.

- [ ] **Step 3: Write implementation**

```go
func renderStatusAllJSON(w io.Writer) error {
	entries, err := registry.List()
	if err != nil { return err }
	live := entries[:0]
	for _, e := range entries {
		if registry.IsAlive(e) { live = append(live, e) } else { registry.Remove(e.Cwd) }
	}
	type row struct {
		Cwd       string    `json:"cwd"`
		Name      string    `json:"name"`
		HubURL    string    `json:"hub_url"`
		Sessions  int       `json:"sessions"`
		StartedAt time.Time `json:"started_at"`
	}
	rows := make([]row, len(live))
	for i, e := range live {
		rows[i] = row{e.Cwd, e.Name, e.HubURL, sessions.CountByWorkspace(e.Cwd), e.StartedAt}
	}
	return json.NewEncoder(w).Encode(struct {
		Workspaces []row `json:"workspaces"`
	}{rows})
}
```

- [ ] **Step 4: Run test**

```bash
go test ./cli/ -run TestStatusAllJSON -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/status.go cli/status_test.go
git commit -m "feat(cli): bones status --all --json output"
```

---

## Phase 6: bones down --all (Task 17)

### Task 17: --all flag teardowns all registered workspaces

**Files:** `cli/down.go`, `cli/down_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestDownAllInvokesPerWorkspace(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	ws1 := t.TempDir()
	ws2 := t.TempDir()
	now := time.Now().UTC()
	registry.Write(registry.Entry{Cwd: ws1, Name: "a", HubPID: os.Getpid(), StartedAt: now})
	registry.Write(registry.Entry{Cwd: ws2, Name: "b", HubPID: os.Getpid(), StartedAt: now})

	cmd := DownCmd{Yes: true, KeepHub: true, All: true} // KeepHub avoids killing real hub
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil { t.Fatalf("Down: %v", err) }

	for _, ws := range []string{ws1, ws2} {
		if _, err := registry.Read(ws); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("expected registry entry removed for %s, got %v", ws, err)
		}
	}
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cli/ -run TestDownAllInvokesPerWorkspace
```
Expected: FAIL — `--all` not handled.

- [ ] **Step 3: Modify `cli/down.go`** to add `All bool` field and handler:

```go
type DownCmd struct {
	Yes        bool `name:"yes" short:"y" help:"skip the confirmation prompt"`
	KeepSkills bool `name:"keep-skills" help:"do not remove .claude/skills"`
	KeepHooks  bool `name:"keep-hooks" help:"do not edit .claude/settings.json"`
	KeepHub    bool `name:"keep-hub" help:"do not stop hub or remove .orchestrator/"`
	DryRun     bool `name:"dry-run" help:"print plan without executing"`
	All        bool `name:"all" help:"tear down all registered workspaces"`
}

func (c *DownCmd) Run(g *libfossilcli.Globals) error {
	if c.All { return c.runAll(g) }
	// ... existing single-workspace path ...
}

func (c *DownCmd) runAll(g *libfossilcli.Globals) error {
	entries, err := registry.List()
	if err != nil { return err }
	if len(entries) == 0 {
		fmt.Println("No workspaces running.")
		return nil
	}

	// Print summary
	fmt.Println("Will stop:")
	for _, e := range entries {
		fmt.Printf("  %-20s %s   sessions=%d\n", e.Name, e.Cwd, sessions.CountByWorkspace(e.Cwd))
	}
	fmt.Printf("\n%d workspaces will be terminated.\n", len(entries))

	if !c.Yes {
		fmt.Print("Continue? [y/N] ")
		var resp string
		fmt.Scanln(&resp)
		if !strings.EqualFold(strings.TrimSpace(resp), "y") {
			fmt.Println("Aborted.")
			return nil
		}
	}

	var firstErr error
	for _, e := range entries {
		single := *c
		single.All = false
		single.Yes = true
		if err := runDown(e.Cwd, &single, os.Stdin); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", e.Name, err)
			if firstErr == nil { firstErr = err }
		}
	}
	return firstErr
}
```

- [ ] **Step 4: Run test**

```bash
go test ./cli/ -run TestDownAllInvokesPerWorkspace -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/down.go cli/down_test.go
git commit -m "feat(cli): bones down --all tears down all registered workspaces"
```

---

## Phase 7: bones env (Tasks 18–21)

### Task 18: walkUpToBones helper

**Files:** `cli/env.go`, `cli/env_test.go`

- [ ] **Step 1: Write the failing test**

```go
// cli/env_test.go
package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWalkUpToBones(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	os.MkdirAll(deep, 0o755)
	bonesDir := filepath.Join(root, "a", ".bones")
	os.MkdirAll(bonesDir, 0o755)

	got, found := walkUpToBones(deep)
	if !found { t.Fatalf("expected to find .bones above %s", deep) }
	if got != filepath.Join(root, "a") { t.Fatalf("got %s, want %s", got, filepath.Join(root, "a")) }

	// No .bones above
	other := t.TempDir()
	if _, found := walkUpToBones(other); found {
		t.Fatalf("expected not found in %s", other)
	}
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cli/ -run TestWalkUpToBones
```
Expected: FAIL `undefined: walkUpToBones`.

- [ ] **Step 3: Write implementation**

```go
// cli/env.go
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// walkUpToBones returns (workspaceRoot, true) if a .bones directory exists at
// startDir or any ancestor; otherwise ("", false).
func walkUpToBones(startDir string) (string, bool) {
	dir := startDir
	for {
		if _, err := os.Stat(filepath.Join(dir, ".bones")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir { return "", false }
		dir = parent
	}
}
```

- [ ] **Step 4: Run test**

```bash
go test ./cli/ -run TestWalkUpToBones -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/env.go cli/env_test.go
git commit -m "feat(cli): add walkUpToBones helper"
```

---

### Task 19: workspace name resolver (basename or override)

**Files:** `cli/env.go`, `cli/env_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestResolveWorkspaceName(t *testing.T) {
	t.Run("basename when no override", func(t *testing.T) {
		root := t.TempDir()
		os.MkdirAll(filepath.Join(root, ".bones"), 0o755)
		got := resolveWorkspaceName(root)
		if got != filepath.Base(root) { t.Fatalf("got %q, want %q", got, filepath.Base(root)) }
	})

	t.Run("override from .bones/workspace_name", func(t *testing.T) {
		root := t.TempDir()
		bones := filepath.Join(root, ".bones")
		os.MkdirAll(bones, 0o755)
		os.WriteFile(filepath.Join(bones, "workspace_name"), []byte("auth-service\n"), 0o644)
		got := resolveWorkspaceName(root)
		if got != "auth-service" { t.Fatalf("got %q, want auth-service", got) }
	})
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cli/ -run TestResolveWorkspaceName
```
Expected: FAIL `undefined: resolveWorkspaceName`.

- [ ] **Step 3: Write implementation**

```go
// resolveWorkspaceName returns the human display name for the workspace at root.
// If .bones/workspace_name exists, returns its trimmed contents; otherwise basename(root).
func resolveWorkspaceName(root string) string {
	data, err := os.ReadFile(filepath.Join(root, ".bones", "workspace_name"))
	if err == nil {
		if name := strings.TrimSpace(string(data)); name != "" { return name }
	}
	return filepath.Base(root)
}
```

- [ ] **Step 4: Run test**

```bash
go test ./cli/ -run TestResolveWorkspaceName -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/env.go cli/env_test.go
git commit -m "feat(cli): resolveWorkspaceName (basename or override)"
```

---

### Task 20: EnvCmd verb (export statements)

**Files:** `cli/env.go`, `cli/env_test.go`, `cmd/bones/cli.go`

- [ ] **Step 1: Write the failing test**

```go
func TestEnvCmdInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".bones"), 0o755)
	t.Chdir(root)

	var buf strings.Builder
	cmd := EnvCmd{Shell: "bash"}
	if err := cmd.run(&buf); err != nil { t.Fatalf("run: %v", err) }

	out := buf.String()
	if !strings.Contains(out, "export BONES_WORKSPACE=") {
		t.Fatalf("missing BONES_WORKSPACE export:\n%s", out)
	}
	if !strings.Contains(out, "export BONES_WORKSPACE_CWD="+root) {
		t.Fatalf("missing BONES_WORKSPACE_CWD export:\n%s", out)
	}
}

func TestEnvCmdOutsideWorkspace(t *testing.T) {
	t.Chdir(t.TempDir()) // a dir with no .bones
	var buf strings.Builder
	cmd := EnvCmd{Shell: "bash"}
	if err := cmd.run(&buf); err != nil { t.Fatalf("run: %v", err) }
	out := buf.String()
	for _, want := range []string{"unset BONES_WORKSPACE", "unset BONES_WORKSPACE_CWD"} {
		if !strings.Contains(out, want) { t.Fatalf("expected %q:\n%s", want, out) }
	}
}

func TestEnvCmdFishSyntax(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".bones"), 0o755)
	t.Chdir(root)
	var buf strings.Builder
	cmd := EnvCmd{Shell: "fish"}
	cmd.run(&buf)
	if !strings.Contains(buf.String(), "set -gx BONES_WORKSPACE ") {
		t.Fatalf("expected fish 'set -gx', got:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cli/ -run TestEnvCmd
```
Expected: FAIL `undefined: EnvCmd`.

- [ ] **Step 3: Write implementation**

```go
type EnvCmd struct {
	Shell string `name:"shell" help:"shell syntax: bash|zsh|fish (default: auto)" enum:"bash,zsh,fish,"`
}

func (c *EnvCmd) Run() error { return c.run(os.Stdout) }

func (c *EnvCmd) run(w io.Writer) error {
	shell := c.Shell
	if shell == "" { shell = detectShell() }

	cwd, err := os.Getwd()
	if err != nil { return err }

	root, found := walkUpToBones(cwd)
	if !found {
		writeUnset(w, shell)
		return nil
	}

	name := resolveWorkspaceName(root)
	writeExport(w, shell, "BONES_WORKSPACE", name)
	writeExport(w, shell, "BONES_WORKSPACE_CWD", root)
	return nil
}

func detectShell() string {
	s := os.Getenv("SHELL")
	switch {
	case strings.HasSuffix(s, "/zsh"): return "zsh"
	case strings.HasSuffix(s, "/fish"): return "fish"
	default: return "bash"
	}
}

func writeExport(w io.Writer, shell, k, v string) {
	if shell == "fish" {
		fmt.Fprintf(w, "set -gx %s %s\n", k, v)
	} else {
		fmt.Fprintf(w, "export %s=%s\n", k, v)
	}
}

func writeUnset(w io.Writer, shell string) {
	for _, k := range []string{"BONES_WORKSPACE", "BONES_WORKSPACE_CWD"} {
		if shell == "fish" {
			fmt.Fprintf(w, "set -e %s\n", k)
		} else {
			fmt.Fprintf(w, "unset %s\n", k)
		}
	}
}
```

Modify `cmd/bones/cli.go` to register:

```go
Env bonescli.EnvCmd `cmd:"" group:"daily" help:"Print shell-export statements for current workspace"`
```

- [ ] **Step 4: Run tests**

```bash
go test ./cli/ -run TestEnvCmd -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/env.go cli/env_test.go cmd/bones/cli.go
git commit -m "feat(cli): add bones env verb (BONES_WORKSPACE shell integration)"
```

---

### Task 21: Workspace_name override file (used by rename and env)

**Files:** `internal/workspace/workspace.go` (or new `internal/workspace/name.go`)

This task adds the read/write helpers for `.bones/workspace_name`. Used by Task 22 (`bones rename`).

- [ ] **Step 1: Write the failing test**

```go
// internal/workspace/name_test.go (new file)
package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadName(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".bones"), 0o755)

	if err := WriteName(root, "auth-service"); err != nil { t.Fatalf("WriteName: %v", err) }

	got, err := ReadName(root)
	if err != nil { t.Fatalf("ReadName: %v", err) }
	if got != "auth-service" { t.Fatalf("got %q, want auth-service", got) }
}

func TestReadNameMissing(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".bones"), 0o755)
	got, err := ReadName(root)
	if err != nil { t.Fatalf("ReadName missing: %v", err) }
	if got != "" { t.Fatalf("expected empty, got %q", got) }
}
```

- [ ] **Step 2: Run test**

```bash
go test ./internal/workspace/ -run TestWriteReadName -run TestReadNameMissing
```
Expected: FAIL `undefined: WriteName, ReadName`.

- [ ] **Step 3: Write implementation**

```go
// internal/workspace/name.go (new file)
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteName persists the workspace_name override at <root>/.bones/workspace_name.
func WriteName(root, name string) error {
	path := filepath.Join(root, ".bones", "workspace_name")
	return os.WriteFile(path, []byte(name+"\n"), 0o644)
}

// ReadName returns the workspace_name override or "" if not set. Returns error
// only on unexpected I/O failures.
func ReadName(root string) (string, error) {
	path := filepath.Join(root, ".bones", "workspace_name")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) { return "", nil }
	if err != nil { return "", fmt.Errorf("workspace_name read: %w", err) }
	return strings.TrimSpace(string(data)), nil
}
```

- [ ] **Step 4: Run test**

```bash
go test ./internal/workspace/ -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/workspace/name.go internal/workspace/name_test.go
git commit -m "feat(workspace): add WriteName/ReadName for workspace_name override"
```

---

## Phase 8: bones rename (Tasks 22–24)

### Task 22: RenameCmd validation

**Files:** `cli/rename.go`, `cli/rename_test.go`

- [ ] **Step 1: Write the failing test**

```go
// cli/rename_test.go
package cli

import (
	"strings"
	"testing"
)

func TestValidateRenameName(t *testing.T) {
	tests := []struct {
		name    string
		want    string // expected error substring; "" = ok
	}{
		{"", "non-empty"},
		{"foo", ""},
		{"foo/bar", "separator"},
		{"foo\\bar", "separator"},
		{"auth-service", ""},
		{strings.Repeat("a", 200), "too long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRenameName(tt.name)
			if tt.want == "" {
				if err != nil { t.Fatalf("expected ok, got %v", err) }
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cli/ -run TestValidateRenameName
```
Expected: FAIL `undefined: validateRenameName`.

- [ ] **Step 3: Write implementation**

```go
// cli/rename.go
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/workspace"
)

func validateRenameName(name string) error {
	if name == "" { return fmt.Errorf("name must be non-empty") }
	if len(name) > 128 { return fmt.Errorf("name too long (max 128 chars)") }
	if strings.ContainsAny(name, "/\\") { return fmt.Errorf("name must not contain path separator (/ or \\)") }
	return nil
}
```

- [ ] **Step 4: Run test**

```bash
go test ./cli/ -run TestValidateRenameName -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/rename.go cli/rename_test.go
git commit -m "feat(cli): add bones rename name validation"
```

---

### Task 23: RenameCmd uniqueness check + write

**Files:** `cli/rename.go`, `cli/rename_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRenameWritesAndUpdatesRegistry(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wsDir := t.TempDir()
	os.MkdirAll(filepath.Join(wsDir, ".bones"), 0o755)
	t.Chdir(wsDir)

	// Seed registry with this workspace
	registry.Write(registry.Entry{Cwd: wsDir, Name: filepath.Base(wsDir), HubPID: os.Getpid(), StartedAt: time.Now().UTC()})

	cmd := RenameCmd{NewName: "auth-service"}
	if err := cmd.Run(); err != nil { t.Fatalf("Run: %v", err) }

	// .bones/workspace_name updated
	got, _ := workspace.ReadName(wsDir)
	if got != "auth-service" { t.Fatalf("workspace_name = %q, want auth-service", got) }

	// registry entry updated
	e, _ := registry.Read(wsDir)
	if e.Name != "auth-service" { t.Fatalf("registry name = %q, want auth-service", e.Name) }
}

func TestRenameRejectsCollision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	otherWS := t.TempDir()
	wsDir := t.TempDir()
	os.MkdirAll(filepath.Join(wsDir, ".bones"), 0o755)
	t.Chdir(wsDir)

	// Another workspace already has the name "taken"
	registry.Write(registry.Entry{Cwd: otherWS, Name: "taken", HubPID: os.Getpid(), StartedAt: time.Now().UTC()})
	registry.Write(registry.Entry{Cwd: wsDir, Name: "self", HubPID: os.Getpid(), StartedAt: time.Now().UTC()})

	cmd := RenameCmd{NewName: "taken"}
	err := cmd.Run()
	if err == nil { t.Fatalf("expected collision error, got nil") }
	if !strings.Contains(err.Error(), "already used by") {
		t.Fatalf("expected 'already used by' in error, got %v", err)
	}
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cli/ -run TestRenameWritesAndUpdatesRegistry -run TestRenameRejectsCollision
```
Expected: FAIL `undefined: RenameCmd`.

- [ ] **Step 3: Write implementation**

```go
type RenameCmd struct {
	NewName string `arg:"" required:""`
}

func (c *RenameCmd) Run() error {
	if err := validateRenameName(c.NewName); err != nil { return err }

	cwd, err := os.Getwd()
	if err != nil { return err }
	root, found := walkUpToBones(cwd)
	if !found { return fmt.Errorf("not inside a bones workspace (no .bones/ found above %s)", cwd) }

	// Uniqueness check across all live workspaces
	entries, err := registry.List()
	if err != nil { return err }
	for _, e := range entries {
		if e.Cwd == root { continue } // skip self
		if e.Name == c.NewName {
			return fmt.Errorf("name %q is already used by workspace at %s\n  (rename that workspace first, or pick a different name)", c.NewName, e.Cwd)
		}
	}

	// Write the override file first (source of truth)
	if err := workspace.WriteName(root, c.NewName); err != nil { return err }

	// Update the registry entry's Name field if this workspace is registered
	if entry, err := registry.Read(root); err == nil {
		entry.Name = c.NewName
		if err := registry.Write(entry); err != nil { return err }
	}

	fmt.Printf("Renamed %s: → %s\n", root, c.NewName)
	return nil
}
```

Modify `cmd/bones/cli.go`:

```go
Rename bonescli.RenameCmd `cmd:"" group:"daily" help:"Set the workspace's display name"`
```

- [ ] **Step 4: Run tests**

```bash
go test ./cli/ -run TestRename -v
```
Expected: PASS (all rename tests).

- [ ] **Step 5: Commit**

```bash
git add cli/rename.go cli/rename_test.go cmd/bones/cli.go
git commit -m "feat(cli): add bones rename verb (with uniqueness check)"
```

---

### Task 24: env consults workspace_name override

**Files:** `cli/env.go`, `cli/env_test.go`

The existing `resolveWorkspaceName` (Task 19) already reads `.bones/workspace_name`. This task confirms it consults the new `workspace.ReadName` helper rather than reading the file directly — and adds a test that exercises the rename → env flow.

- [ ] **Step 1: Write the failing test**

```go
func TestEnvUsesRenamedName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".bones"), 0o755)
	t.Chdir(root)

	// Simulate a rename
	workspace.WriteName(root, "auth-service")

	var buf strings.Builder
	(&EnvCmd{Shell: "bash"}).run(&buf)
	if !strings.Contains(buf.String(), "export BONES_WORKSPACE=auth-service") {
		t.Fatalf("expected renamed name in env output:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run test**

```bash
go test ./cli/ -run TestEnvUsesRenamedName
```
Expected: PASS if Task 19's `resolveWorkspaceName` reads the file directly. May FAIL if `resolveWorkspaceName` was implemented differently than the workspace.ReadName helper.

- [ ] **Step 3: If failing, refactor `resolveWorkspaceName`**

```go
import "github.com/danmestas/bones/internal/workspace"

func resolveWorkspaceName(root string) string {
	if name, err := workspace.ReadName(root); err == nil && name != "" { return name }
	return filepath.Base(root)
}
```

- [ ] **Step 4: Run test**

```bash
go test ./cli/ -run TestEnv -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/env.go cli/env_test.go
git commit -m "refactor(cli): env uses workspace.ReadName for rename consistency"
```

---

## Phase 9: Hub /health endpoint, README, ADR footnote (Tasks 25–28)

### Task 25: Confirm or add /health endpoint to hub

**Files:** `internal/hub/hub.go` (or wherever hub HTTP server is configured), test alongside.

- [ ] **Step 1: Verify or write the failing test**

Search the hub package for `/health`:
```bash
grep -rn "/health" internal/hub/
```

If `/health` already exists, this task becomes a no-op verification. If not:

```go
// internal/hub/hub_test.go (append)
func TestHubHealthEndpoint(t *testing.T) {
	// Use existing test fixture for hub HTTP server
	srv, cleanup := newTestHubServer(t) // helper from existing tests
	defer cleanup()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil { t.Fatalf("GET /health: %v", err) }
	defer resp.Body.Close()
	if resp.StatusCode != 200 { t.Fatalf("status = %d, want 200", resp.StatusCode) }
}
```

- [ ] **Step 2: Run test**

```bash
go test ./internal/hub/ -run TestHubHealthEndpoint
```
Expected: FAIL if endpoint missing; PASS otherwise.

- [ ] **Step 3: If failing, add the endpoint**

In whichever file registers HTTP routes (look for `http.HandleFunc`, `mux.Handle`, etc.):

```go
mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})
```

- [ ] **Step 4: Run test**

```bash
go test ./internal/hub/ -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/hub/
git commit -m "feat(hub): add /health endpoint for registry stale detection"
```

---

### Task 26: README — shell prompt-hook snippets

**Files:** `README.md`

- [ ] **Step 1: Identify section**

Open `README.md` and find an appropriate location (likely after "Installation" or in a "Shell Integration" section). If no shell-integration section exists, add one.

- [ ] **Step 2: Add content**

Append/insert:

````markdown
## Shell prompt integration

`bones env` prints shell-export statements for `BONES_WORKSPACE` and `BONES_WORKSPACE_CWD`. Wire it into your shell's prompt hook so the env vars stay in sync as you `cd` between workspaces:

### zsh (`~/.zshrc`)

```zsh
_bones_env_hook() { eval "$(bones env)"; }
typeset -ag precmd_functions
precmd_functions+=(_bones_env_hook)
```

### bash (`~/.bashrc`)

```bash
PROMPT_COMMAND='eval "$(bones env)"'
```

### fish (`~/.config/fish/conf.d/bones.fish`)

```fish
function _bones_env_hook --on-event fish_prompt
    bones env --shell=fish | source
end
```

## Prompt theme integration

Once `BONES_WORKSPACE` is exported, themes read it directly.

### Starship (`~/.config/starship.toml`)

```toml
[env_var.BONES_WORKSPACE]
format = "[$env_value](bold yellow) "
```

### Powerlevel10k

Define a custom segment that reads `$BONES_WORKSPACE`:

```zsh
function prompt_bones_workspace() {
    [[ -n "$BONES_WORKSPACE" ]] && p10k segment -t "$BONES_WORKSPACE" -f yellow
}
```

Then add `bones_workspace` to your `POWERLEVEL9K_RIGHT_PROMPT_ELEMENTS` (or `_LEFT_`).

### Plain bash/zsh

```bash
PS1='[${BONES_WORKSPACE:-no-bones}] \w $ '
```
````

- [ ] **Step 3: Verify rendering**

Open README.md in a markdown previewer to confirm code blocks render correctly.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs(readme): add shell prompt-hook + theme snippets for BONES_WORKSPACE"
```

---

### Task 27: README — bones status --all, down --all, rename usage

**Files:** `README.md`

- [ ] **Step 1: Add usage examples**

In an appropriate section (after the existing `bones status` mention or under "Common workflows"):

````markdown
## Cross-workspace commands

When you're running bones across multiple workspaces (e.g., 6 terminals on 3 different repos), these commands give you a global view from any terminal:

```
$ bones status --all
WORKSPACE          PATH                HUB    SESSIONS  UPTIME
foo                ~/projects/foo      :8765  6         2h
bar                ~/projects/bar      :8766  3         45m
auth-service       ~/work/auth         :8767  1         10m

$ bones down --all      # tear down every registered workspace (prompts unless --yes)
$ bones rename auth-service   # set this workspace's display name
```

The registry that backs these commands lives at `~/.bones/workspaces/` (one JSON file per running workspace; created by `bones up`, removed by `bones down`).
````

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs(readme): document bones status --all, down --all, rename"
```

---

### Task 28: ADR 0038 footnote pointing to Spec 1's Future Direction

**Files:** `docs/adr/0038-per-workspace-hub-ports.md`

- [ ] **Step 1: Read the ADR to find the right insertion point**

```bash
cat docs/adr/0038-per-workspace-hub-ports.md
```

Find the "Alternatives considered" section (which contains the "single global hub multiplexing all workspaces" rejection).

- [ ] **Step 2: Add footnote**

Append after the "Alternatives considered" rejection text, on a new line:

```markdown
> **Future supersession trigger:** The "cross-workspace NATS subject routing" objection is solvable via JetStream `domain=` and per-account isolation. See `docs/superpowers/specs/2026-05-01-cross-workspace-identity-design.md` (Future Direction section) for the leaf-node topology that revisits this decision when cross-machine, real-time, or multi-user becomes a forcing function.
```

- [ ] **Step 3: Commit**

```bash
git add docs/adr/0038-per-workspace-hub-ports.md
git commit -m "docs(adr): footnote ADR 0038 with leaf-node future-supersession trigger"
```

---

## Final verification

- [ ] **Step F1: Run full local CI**

```bash
make check
```
Expected: `check: OK` (no errors).

- [ ] **Step F2: Push to PR**

```bash
git push
```

- [ ] **Step F3: Verify remote CI**

```bash
gh pr checks 107
```
Expected: all checks pass (or only docs-only skip).

---

## Spec coverage check

| Spec section | Plan task(s) |
|---|---|
| Registry — one file per workspace | 1, 2, 3, 4, 5, 6 |
| Registry — atomic write (tmp+rename) | 3 |
| Registry — stale detection (PID + HTTP) | 7 |
| Sessions — one file per session | 8 |
| Sessions — register/unregister via hidden CLI | 11, 12 |
| Sessions — orphan GC (PID-alive filter) | 9 |
| `bones up` registers workspace | 13 |
| `bones down` removes registry entry | 14 |
| `bones status --all` | 15, 16 |
| `bones down --all` | 17 |
| `bones env` (BONES_WORKSPACE shell integration) | 18, 19, 20, 24 |
| `bones rename` | 21, 22, 23 |
| README snippets (zsh/bash/fish/starship/p10k) | 26, 27 |
| ADR 0038 footnote | 28 |
| `/health` endpoint (required by stale detection) | 25 |

All spec sections covered.
