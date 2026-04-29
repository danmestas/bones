package dispatch

// Spec is the parent-resolved description of a task ready to dispatch
// to a worker process. Pure data; no substrate references. The CLI
// adapter that produced the Task value is the only thing that knows
// about coord.
type Spec struct {
	TaskID        string
	Title         string
	Files         []string
	Thread        string
	ParentAgentID string
	WorkerAgentID string
	WorkspaceDir  string
}

// BuildSpec materializes a Spec from a parent agent's view of a
// dispatchable task. The Task interface keeps dispatch coord-free;
// CLI callers pass a small adapter wrapping coord.Task.
func BuildSpec(parentAgentID, workspaceDir string, task Task) (Spec, error) {
	taskID := task.ID()
	srcFiles := task.Files()
	files := make([]string, len(srcFiles))
	copy(files, srcFiles)
	return Spec{
		TaskID:        taskID,
		Title:         task.Title(),
		Files:         files,
		Thread:        taskID,
		ParentAgentID: parentAgentID,
		WorkerAgentID: parentAgentID + "/" + taskID,
		WorkspaceDir:  workspaceDir,
	}, nil
}
