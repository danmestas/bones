package dispatch

import "github.com/danmestas/bones/coord"

type Spec struct {
	TaskID        coord.TaskID
	Title         string
	Files         []string
	Thread        string
	ParentAgentID string
	WorkerAgentID string
	WorkspaceDir  string
}

func BuildSpec(parentAgentID, workspaceDir string, task coord.Task) (Spec, error) {
	return Spec{
		TaskID:        task.ID(),
		Title:         task.Title(),
		Files:         task.Files(),
		Thread:        string(task.ID()),
		ParentAgentID: parentAgentID,
		WorkerAgentID: parentAgentID + "/" + string(task.ID()),
		WorkspaceDir:  workspaceDir,
	}, nil
}
