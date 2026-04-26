package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// runUp performs a full single-command bootstrap from a fresh clone:
//  1. workspace init (idempotent — skips if already initialized)
//  2. orchestrator scaffold (scripts, skills, hooks)
//  3. build bin/leaf if missing
//  4. run hub-bootstrap.sh and verify the hub is up
//
// runUp is designed for Option B from the DX audit: a new `agent-init up`
// subcommand that wraps the four steps newcomers currently run manually.
func runUp(cwd string) error {
	// Step 1: workspace.
	// Init creates the workspace; if it already exists we fall through silently.
	wsDir, err := ensureWorkspace(cwd)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	fmt.Printf("up: workspace at %s\n", wsDir)

	// Step 2: orchestrator scaffold.
	if err := runOrchestrator(wsDir); err != nil {
		return fmt.Errorf("orchestrator scaffold: %w", err)
	}
	fmt.Println("up: orchestrator scripts, skills, and hooks installed")

	// Step 3: ensure bin/leaf.
	if err := ensureLeafBinary(wsDir); err != nil {
		return fmt.Errorf("bin/leaf: %w", err)
	}

	// Step 4: hub bootstrap.
	if err := runHubBootstrap(wsDir); err != nil {
		return fmt.Errorf("hub-bootstrap: %w", err)
	}

	// Step 5: verify hub reachable.
	if err := waitHubReady("http://127.0.0.1:8765/xfer", 5*time.Second); err != nil {
		return fmt.Errorf("hub health check: %w", err)
	}
	fmt.Println("up: hub is up at http://127.0.0.1:8765 — NATS at nats://127.0.0.1:4222")
	return nil
}

// ensureWorkspace initializes a workspace at cwd if none exists, or walks up to
// find an existing one. Returns the workspace root directory.
func ensureWorkspace(cwd string) (string, error) {
	// Try join first (fast, no side effects).
	if wsDir := findWorkspaceDir(cwd); wsDir != "" {
		return wsDir, nil
	}
	// Fresh clone path: init in cwd.
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return "", err
	}
	markerDir := filepath.Join(cwd, ".agent-infra")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return "", err
	}
	return cwd, nil
}

// findWorkspaceDir walks up from dir looking for the .agent-infra marker.
// Returns "" if not found.
func findWorkspaceDir(dir string) string {
	cur := dir
	for {
		if _, err := os.Stat(filepath.Join(cur, ".agent-infra")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// ensureLeafBinary checks the standard leaf-binary locations used by
// hub-bootstrap.sh. If the binary is missing it attempts to build it from the
// sibling EdgeSync repo. Matches hub-bootstrap.sh's resolution order exactly:
//  1. $LEAF_BIN env var
//  2. $ROOT/bin/leaf
//  3. $EDGESYNC_DIR/bin/leaf  ($EDGESYNC_DIR defaults to $ROOT/../EdgeSync)
//  4. `leaf` on $PATH
//  5. Build from $EDGESYNC_DIR/leaf/cmd/leaf  (sibling clone must exist)
func ensureLeafBinary(root string) error {
	// 1. Explicit override.
	if p := os.Getenv("LEAF_BIN"); p != "" {
		if isExec(p) {
			fmt.Printf("up: using LEAF_BIN=%s\n", p)
			return nil
		}
		return fmt.Errorf("LEAF_BIN=%s is not executable", p)
	}

	// 2. $ROOT/bin/leaf
	rootLeaf := filepath.Join(root, "bin", "leaf")
	if isExec(rootLeaf) {
		fmt.Printf("up: found bin/leaf at %s\n", rootLeaf)
		return nil
	}

	// 3. $EDGESYNC_DIR/bin/leaf
	edgesyncDir := os.Getenv("EDGESYNC_DIR")
	if edgesyncDir == "" {
		edgesyncDir = filepath.Join(root, "..", "EdgeSync")
	}
	edgesyncLeaf := filepath.Join(edgesyncDir, "bin", "leaf")
	if isExec(edgesyncLeaf) {
		fmt.Printf("up: found leaf at %s\n", edgesyncLeaf)
		return nil
	}

	// 4. $PATH
	if p, err := exec.LookPath("leaf"); err == nil {
		fmt.Printf("up: found leaf on PATH: %s\n", p)
		return nil
	}

	// 5. Build from sibling EdgeSync repo.
	leafSrc := filepath.Join(edgesyncDir, "leaf", "cmd", "leaf")
	if _, err := os.Stat(leafSrc); os.IsNotExist(err) {
		return fmt.Errorf(
			"EdgeSync sibling clone not found at %s — "+
				"clone https://github.com/danmestas/EdgeSync next to agent-infra, "+
				"then re-run `agent-init up`",
			edgesyncDir,
		)
	}
	fmt.Printf("up: building bin/leaf in %s ...\n", edgesyncDir)
	cmd := exec.Command("make", "leaf")
	cmd.Dir = edgesyncDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("make leaf in %s: %w", edgesyncDir, err)
	}
	if !isExec(edgesyncLeaf) {
		return fmt.Errorf("make leaf succeeded but %s still not executable", edgesyncLeaf)
	}
	fmt.Printf("up: built %s\n", edgesyncLeaf)
	return nil
}

// runHubBootstrap execs hub-bootstrap.sh from the workspace root. The script
// is idempotent; if the hub is already up it prints a notice and returns 0.
func runHubBootstrap(root string) error {
	script := filepath.Join(root, ".orchestrator", "scripts", "hub-bootstrap.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("hub-bootstrap.sh not found at %s "+
			"(run orchestrator scaffold first)", script)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("hub-bootstrap.sh: %w", err)
	}
	return nil
}

// waitHubReady polls the hub's /xfer endpoint until it responds or the timeout
// elapses. A 405 (method not allowed) counts as "up" — /xfer only accepts
// POST, so a GET returning 405 proves the listener is live.
func waitHubReady(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx // fire-and-forget health probe
		if err == nil {
			_ = resp.Body.Close()
			// 200 or 405 both mean the HTTP server is live.
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusMethodNotAllowed {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("hub at %s did not become ready within %s", url, timeout)
}

// isExec reports whether path exists and has the owner-exec bit set.
func isExec(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&0o100 != 0
}
