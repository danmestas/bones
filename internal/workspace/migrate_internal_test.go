package workspace

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDetectLegacyLayout_Absent(t *testing.T) {
	dir := t.TempDir()
	state, err := detectLegacyLayout(dir)
	if err != nil {
		t.Fatalf("detectLegacyLayout: %v", err)
	}
	if state != legacyAbsent {
		t.Errorf("got state %v, want legacyAbsent", state)
	}
}

func TestDetectLegacyLayout_DeadLeaf(t *testing.T) {
	dir := t.TempDir()
	orchDir := filepath.Join(dir, ".orchestrator")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	state, err := detectLegacyLayout(dir)
	if err != nil {
		t.Fatalf("detectLegacyLayout: %v", err)
	}
	if state != legacyDead {
		t.Errorf("got state %v, want legacyDead", state)
	}
}

func TestDetectLegacyLayout_LiveLeaf(t *testing.T) {
	dir := t.TempDir()
	pidDir := filepath.Join(dir, ".orchestrator", "pids")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Use the test process's own pid — guaranteed live.
	livePID := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(filepath.Join(pidDir, "fossil.pid"),
		[]byte(livePID), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	state, err := detectLegacyLayout(dir)
	if err != nil {
		t.Fatalf("detectLegacyLayout: %v", err)
	}
	if state != legacyLive {
		t.Errorf("got state %v, want legacyLive", state)
	}
}

func TestMigrateLegacyLayout_MovesFiles(t *testing.T) {
	dir := t.TempDir()
	// Build a synthetic legacy layout.
	orchDir := filepath.Join(dir, ".orchestrator")
	bonesDir := filepath.Join(dir, ".bones")
	pidsDir := filepath.Join(orchDir, "pids")
	for _, d := range []string{
		pidsDir,
		filepath.Join(orchDir, "nats-store", "jetstream"),
		filepath.Join(orchDir, "scripts"),
		bonesDir, // pre-existing legacy workspace-leaf state
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(orchDir, "hub.fossil"):                  "fossil-bytes",
		filepath.Join(orchDir, "hub-fossil-url"):              "http://127.0.0.1:8765",
		filepath.Join(orchDir, "hub-nats-url"):                "nats://127.0.0.1:4222",
		filepath.Join(orchDir, "fossil.log"):                  "fossil-log-bytes",
		filepath.Join(orchDir, "nats.log"):                    "nats-log-bytes",
		filepath.Join(orchDir, "hub.log"):                     "hub-log-bytes",
		filepath.Join(orchDir, "scripts", "hub-bootstrap.sh"): "#!/bin/sh\n",
		filepath.Join(bonesDir, "config.json"):                `{"agent_id":"old-agent-1234"}`,
		filepath.Join(bonesDir, "repo.fossil"):                "old-substrate",
		filepath.Join(bonesDir, "leaf.pid"):                   "99999",
		filepath.Join(bonesDir, "leaf.log"):                   "old-leaf-log",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}

	if err := migrateLegacyLayout(dir); err != nil {
		t.Fatalf("migrateLegacyLayout: %v", err)
	}

	// .orchestrator/ should be gone.
	if _, err := os.Stat(orchDir); !os.IsNotExist(err) {
		t.Errorf(".orchestrator/ still exists: %v", err)
	}
	// All hub state should be under .bones/.
	for _, p := range []string{
		"hub.fossil", "hub-fossil-url", "hub-nats-url",
		"fossil.log", "nats.log", "hub.log",
		filepath.Join("nats-store", "jetstream"),
	} {
		if _, err := os.Stat(filepath.Join(bonesDir, p)); err != nil {
			t.Errorf(".bones/%s missing after migrate: %v", p, err)
		}
	}
	// Legacy workspace-leaf files should be gone.
	for _, p := range []string{"config.json", "repo.fossil", "leaf.pid", "leaf.log"} {
		if _, err := os.Stat(filepath.Join(bonesDir, p)); !os.IsNotExist(err) {
			t.Errorf(".bones/%s still exists after migrate: %v", p, err)
		}
	}
	// agent.id should carry forward from legacy config.json.
	got, err := readAgentID(dir)
	if err != nil {
		t.Fatalf("readAgentID after migrate: %v", err)
	}
	if got != "old-agent-1234" {
		t.Errorf("agent.id = %q, want %q (carried from old config.json)", got, "old-agent-1234")
	}
}

func TestMigrateLegacyLayout_Idempotent(t *testing.T) {
	dir := t.TempDir()
	orchDir := filepath.Join(dir, ".orchestrator")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orchDir, "hub.fossil"),
		[]byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// First run.
	if err := migrateLegacyLayout(dir); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	// Second run: should be a no-op (legacyAbsent).
	if err := migrateLegacyLayout(dir); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
