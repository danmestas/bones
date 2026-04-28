package dispatch

import (
	"os/exec"
)

// BuildWorkerCommand assembles the `bones tasks dispatch worker` invocation
// with the required flags wired from spec. The parent appends per-result
// flags (--result, --summary, --claim-from-agent-id) before Start.
//
// Earlier iterations passed these as AGENT_INFRA_* env vars, but the
// worker's Kong struct never read them — the worker would fail Kong's
// required-flag validation immediately and the parent would hit its
// 5s subscribe timeout. Flags-only is the single source of truth.
func BuildWorkerCommand(bin string, spec Spec) (*exec.Cmd, error) {
	cmd := exec.Command(bin, "tasks", "dispatch", "worker",
		"--task-id="+string(spec.TaskID),
		"--task-thread="+spec.Thread,
		"--worker-agent-id="+spec.WorkerAgentID,
	)
	cmd.Dir = spec.WorkspaceDir
	return cmd, nil
}
