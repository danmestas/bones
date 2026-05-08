package tasks

// Tally is the per-status summary that both `bones status` and
// `bones tasks status` render. Counts are derived by replaying events
// (not by walking the KV) so the two surfaces share one source. Per
// ADR 0052 §"TaskTally — single source for counts".
type Tally struct {
	// Open is the count of tasks currently in StatusOpen.
	Open int
	// Claimed is the count of tasks currently in StatusClaimed.
	Claimed int
	// Closed is the count of tasks currently in StatusClosed.
	Closed int
	// Total is Open+Claimed+Closed.
	Total int
}

// TaskTally derives a Tally by replaying events into a per-task status
// projection and bucket-counting at the end. The events slice should
// be the full log (or a subset that includes the latest state-changing
// event for each task ID); ordering by stream sequence is the caller's
// responsibility — recovery and live consumers receive ordered events
// by construction.
//
// Events with unknown types are ignored. A task is counted under its
// most recent terminal status; a closed task that was previously
// reopened (not legal under the current DAG, but defensive) reports
// the latest status seen.
func TaskTally(events []EventEnvelope) Tally {
	statuses := make(map[string]Status)
	for _, env := range events {
		switch env.Type {
		case EventTypeCreated:
			if _, ok := statuses[env.TaskID]; !ok {
				statuses[env.TaskID] = StatusOpen
			}
		case EventTypeClaimed:
			statuses[env.TaskID] = StatusClaimed
		case EventTypeUnclaimed:
			statuses[env.TaskID] = StatusOpen
		case EventTypeClosed:
			statuses[env.TaskID] = StatusClosed
		}
	}
	t := Tally{}
	for _, s := range statuses {
		switch s {
		case StatusOpen:
			t.Open++
		case StatusClaimed:
			t.Claimed++
		case StatusClosed:
			t.Closed++
		}
		t.Total++
	}
	return t
}

// RecentActivityCount is the default number of events `bones status`
// surfaces under "Recent activity" per ADR 0052. Tunable as a constant
// rather than a flag — operators wanting more reach for `bones tasks
// watch --since=<duration>`.
const RecentActivityCount = 20
