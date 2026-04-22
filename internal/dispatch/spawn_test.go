package dispatch

import (
	"os"
	"strings"
	"testing"
)

func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func TestBuildWorkerCommand_EncodesSpecForChildProcess(t *testing.T) {
	spec := Spec{
		TaskID:        "agent-infra-abc12345",
		Title:         "dispatch me",
		Files:         []string{"/repo/a.go"},
		Thread:        "agent-infra-abc12345",
		ParentAgentID: "parent-agent",
		WorkerAgentID: "parent-agent/agent-infra-abc12345",
		WorkspaceDir:  "/workspace",
	}
	cmd, err := BuildWorkerCommand("/tmp/agent-tasks", spec)
	if err != nil {
		t.Fatalf("BuildWorkerCommand: %v", err)
	}
	if got := strings.Join(cmd.Args, " "); !strings.Contains(got, "dispatch worker") {
		t.Fatalf("Args=%q, want dispatch worker", got)
	}
	if cmd.Dir != "/workspace" {
		t.Fatalf("Dir=%q", cmd.Dir)
	}
	if !hasEnv(cmd.Env, "AGENT_INFRA_WORKER_AGENT_ID=parent-agent/agent-infra-abc12345") {
		t.Fatalf("Env missing worker agent id: %#v", cmd.Env)
	}
}

func TestSpawnWorker_StartsProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		os.Exit(0)
	}
	spec := Spec{
		TaskID:        "agent-infra-abc12345",
		Title:         "dispatch me",
		Thread:        "agent-infra-abc12345",
		ParentAgentID: "parent-agent",
		WorkerAgentID: "parent-agent/agent-infra-abc12345",
		WorkspaceDir:  t.TempDir(),
	}
	cmd, err := BuildWorkerCommand(os.Args[0], spec)
	if err != nil {
		t.Fatalf("BuildWorkerCommand: %v", err)
	}
	cmd.Env = append(cmd.Env, "GO_WANT_HELPER_PROCESS=1")
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
