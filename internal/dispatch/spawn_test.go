package dispatch

import (
	"os"
	"strings"
	"testing"
)

func TestBuildWorkerCommand_EncodesSpecForChildProcess(t *testing.T) {
	spec := Spec{
		TaskID:        "bones-abc12345",
		Title:         "dispatch me",
		Files:         []string{"/repo/a.go"},
		Thread:        "bones-abc12345",
		ParentAgentID: "parent-agent",
		WorkerAgentID: "parent-agent/bones-abc12345",
		WorkspaceDir:  "/workspace",
	}
	cmd, err := BuildWorkerCommand("/tmp/agent-tasks", spec)
	if err != nil {
		t.Fatalf("BuildWorkerCommand: %v", err)
	}
	got := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"dispatch worker",
		"--task-id=bones-abc12345",
		"--task-thread=bones-abc12345",
		"--worker-agent-id=parent-agent/bones-abc12345",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Args=%q missing %q", got, want)
		}
	}
	if cmd.Dir != "/workspace" {
		t.Fatalf("Dir=%q", cmd.Dir)
	}
}

func TestSpawnWorker_StartsProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		os.Exit(0)
	}
	spec := Spec{
		TaskID:        "bones-abc12345",
		Title:         "dispatch me",
		Thread:        "bones-abc12345",
		ParentAgentID: "parent-agent",
		WorkerAgentID: "parent-agent/bones-abc12345",
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
