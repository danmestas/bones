package dispatch

import (
	"os"
	"os/exec"
)

func BuildWorkerCommand(bin string, spec Spec) (*exec.Cmd, error) {
	cmd := exec.Command(bin, "dispatch", "worker")
	cmd.Dir = spec.WorkspaceDir
	cmd.Env = append(os.Environ(),
		"AGENT_INFRA_TASK_ID="+string(spec.TaskID),
		"AGENT_INFRA_TASK_THREAD="+spec.Thread,
		"AGENT_INFRA_TASK_TITLE="+spec.Title,
		"AGENT_INFRA_WORKER_AGENT_ID="+spec.WorkerAgentID,
		"AGENT_INFRA_PARENT_AGENT_ID="+spec.ParentAgentID,
	)
	return cmd, nil
}
