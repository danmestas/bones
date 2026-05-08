package schemas

//go:generate go run ../../cmd/bones-schemagen -out ../../schemas

import "github.com/danmestas/bones/internal/timefmt"

// Verb identifies one CLI emit site. The dotted name matches the
// command path (`tasks.list` ↔ `bones tasks list`); CurrentVersion is
// the version this binary emits.
//
// All verbs start at "v1" per ADR 0053's hard-cut migration policy.
type VerbInfo struct {
	Verb           string
	CurrentVersion string
	// PayloadName is the Go type name of the payload struct,
	// matched against the schemagen reflector's output. Lets the
	// generator drive entirely off this slice instead of walking
	// the AST.
	PayloadName string
}

// Verbs is the canonical registry. Order is alphabetical by verb
// name; the generator iterates this slice and writes
// schemas/<verb>.<version>.json per entry.
//
// When adding a new verb: append an entry here, define the payload
// struct below, run `make schemas`, and add the per-verb test in
// cli/schemas_test.go.
var Verbs = []VerbInfo{
	{Verb: "doctor", CurrentVersion: "v1", PayloadName: "DoctorAllPayload"},
	{Verb: "status", CurrentVersion: "v1", PayloadName: "StatusAllPayload"},
	{Verb: "swarm.dispatch", CurrentVersion: "v1", PayloadName: "SwarmDispatchPayload"},
	{Verb: "swarm.status", CurrentVersion: "v1", PayloadName: "SwarmStatusPayload"},
	{Verb: "swarm.tasks", CurrentVersion: "v1", PayloadName: "SwarmTasksPayload"},
	{Verb: "tasks.aggregate", CurrentVersion: "v1", PayloadName: "TasksAggregatePayload"},
	{Verb: "tasks.bySlot", CurrentVersion: "v1", PayloadName: "TasksBySlotPayload"},
	{Verb: "tasks.claim", CurrentVersion: "v1", PayloadName: "TasksClaimPayload"},
	{Verb: "tasks.close", CurrentVersion: "v1", PayloadName: "TasksClosePayload"},
	{Verb: "tasks.create", CurrentVersion: "v1", PayloadName: "TasksCreatePayload"},
	{Verb: "tasks.link", CurrentVersion: "v1", PayloadName: "TasksLinkPayload"},
	{Verb: "tasks.list", CurrentVersion: "v1", PayloadName: "TasksListPayload"},
	{Verb: "tasks.prime", CurrentVersion: "v1", PayloadName: "TasksPrimePayload"},
	{Verb: "tasks.ready", CurrentVersion: "v1", PayloadName: "TasksReadyPayload"},
	{Verb: "tasks.show", CurrentVersion: "v1", PayloadName: "TasksShowPayload"},
	{Verb: "tasks.update", CurrentVersion: "v1", PayloadName: "TasksUpdatePayload"},
	{Verb: "workspaces.get", CurrentVersion: "v1", PayloadName: "WorkspacesGetPayload"},
	{Verb: "workspaces.list", CurrentVersion: "v1", PayloadName: "WorkspacesListPayload"},
}

// VersionFor returns the current version string for verb. Falls
// back to "v1" for unknown verbs so the generator and emitters
// share one lookup. (CI gates that every emitter's verb is in this
// registry, so the fallback only fires on coding errors.)
func VersionFor(verb string) string {
	for _, v := range Verbs {
		if v.Verb == verb {
			return v.CurrentVersion
		}
	}
	return "v1"
}

// --- payload structs (one per verb, alphabetical by verb name) ---

// DoctorAllPayload is the payload for `bones doctor --all --json`.
// Mirrors the per-workspace row shape `renderDoctorAllJSON` emits.
type DoctorAllPayload struct {
	Workspaces []DoctorWorkspaceRow `json:"workspaces"`
}

// DoctorWorkspaceRow is one row of the doctor --all summary.
type DoctorWorkspaceRow struct {
	Name     string `json:"name"`
	Cwd      string `json:"cwd"`
	HubAlive bool   `json:"hub_alive"`
	Issues   int    `json:"issues"`
}

// StatusAllPayload is the payload for `bones status --all --json`.
// Mirrors `renderStatusAllJSON`: live registry rows only.
type StatusAllPayload struct {
	Workspaces []StatusWorkspaceRow `json:"workspaces"`
}

// StatusWorkspaceRow is one row of the status --all view.
type StatusWorkspaceRow struct {
	Cwd       string             `json:"cwd"`
	Name      string             `json:"name"`
	HubURL    string             `json:"hub_url"`
	Sessions  int                `json:"sessions"`
	StartedAt timefmt.LoggedTime `json:"started_at"`
}

// SwarmDispatchPayload is the payload for `bones swarm dispatch --json`.
// Mirrors `dispatchSummaryJSON`: manifest path + plan hash + task
// count for the just-built dispatch.
type SwarmDispatchPayload struct {
	ManifestPath string `json:"manifest_path"`
	TaskCount    int    `json:"task_count"`
	PlanSHA256   string `json:"plan_sha256"`
}

// SwarmStatusPayload is the payload for `bones swarm status --json`.
// Mirrors `[]statusRow` — every active swarm-session record.
type SwarmStatusPayload []SwarmStatusRow

// SwarmStatusRow is one swarm-session row.
type SwarmStatusRow struct {
	Slot         string             `json:"slot"`
	TaskID       string             `json:"task_id"`
	AgentID      string             `json:"agent_id"`
	Host         string             `json:"host"`
	LeafPID      int                `json:"leaf_pid"`
	StartedAt    timefmt.LoggedTime `json:"started_at"`
	LastRenewed  timefmt.LoggedTime `json:"last_renewed"`
	State        string             `json:"state"`
	StaleSeconds int64              `json:"stale_seconds"`
}

// SwarmTasksPayload is the payload for `bones swarm tasks --json`.
// Mirrors the `[]tasks.Task` shape returned by the slot-scoped
// readiness filter.
type SwarmTasksPayload []Task

// TasksAggregatePayload is the payload for `bones tasks aggregate --json`.
// Mirrors `aggregateResult`: window summary + per-slot breakdown.
type TasksAggregatePayload struct {
	Since       string               `json:"since"`
	TotalTasks  int                  `json:"total_tasks"`
	TotalSlots  int                  `json:"total_slots"`
	ActiveSlots int                  `json:"active_slots"`
	Slots       []TasksAggregateSlot `json:"slots"`
}

// TasksAggregateSlot is the per-slot summary row inside a tasks.aggregate payload.
type TasksAggregateSlot struct {
	SlotID string   `json:"slot_id"`
	Tasks  int      `json:"tasks"`
	Files  []string `json:"files"`
	Status string   `json:"status"`
}

// TasksBySlotPayload is the payload for `bones tasks list --by-slot --json`.
// Distinct verb name from `tasks.list` because the shape diverges:
// `tasks.list` emits `[]Task`; `tasks.bySlot` emits a per-slot
// grouping with the hot-threshold context.
type TasksBySlotPayload struct {
	Slots        []TasksSlotGroup `json:"slots"`
	HotThreshold int              `json:"hot_threshold"`
}

// TasksSlotGroup is one slot's grouping row.
type TasksSlotGroup struct {
	Slot      string   `json:"slot"`
	OpenCount int      `json:"open_count"`
	Hot       bool     `json:"hot"`
	TaskIDs   []string `json:"task_ids"`
}

// TasksClaimPayload is the payload for `bones tasks claim --json`.
// Emits the post-claim Task record.
type TasksClaimPayload = Task

// TasksClosePayload is the payload for `bones tasks close --json`.
// Emits the post-close Task record.
type TasksClosePayload = Task

// TasksCreatePayload is the payload for `bones tasks create --json`.
// Emits the just-created Task record.
type TasksCreatePayload = Task

// TasksLinkPayload is the payload for `bones tasks link --json`.
// Emits a link-confirmation tuple.
type TasksLinkPayload struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// TasksListPayload is the payload for `bones tasks list --json`.
// Emits a (possibly filtered) list of tasks. Default-mode list only;
// the `--by-slot` mode emits the `tasks.bySlot` shape instead.
type TasksListPayload []Task

// TasksPrimePayload is the payload for `bones tasks prime --json`.
// Mirrors `primeResultJSON`: the agent's open/ready/claimed task
// view + recent threads + online peers.
type TasksPrimePayload struct {
	OpenTasks    []TasksPrimeTask     `json:"open_tasks"`
	ReadyTasks   []TasksPrimeTask     `json:"ready_tasks"`
	ClaimedTasks []TasksPrimeTask     `json:"claimed_tasks"`
	Threads      []TasksPrimeThread   `json:"threads"`
	Peers        []TasksPrimePresence `json:"peers"`
}

// TasksPrimeTask is the trimmed task shape used inside a tasks.prime
// payload — coord.Task projection without storage-internal fields.
type TasksPrimeTask struct {
	ID        string             `json:"id"`
	Title     string             `json:"title"`
	Files     []string           `json:"files,omitempty"`
	ClaimedBy string             `json:"claimed_by,omitempty"`
	CreatedAt timefmt.LoggedTime `json:"created_at"`
	UpdatedAt timefmt.LoggedTime `json:"updated_at"`
}

// TasksPrimeThread is the chat-thread row inside a tasks.prime payload.
type TasksPrimeThread struct {
	ThreadShort  string             `json:"thread_short"`
	LastActivity timefmt.LoggedTime `json:"last_activity"`
	MessageCount int                `json:"message_count"`
	LastBody     string             `json:"last_body"`
}

// TasksPrimePresence is the peer-presence row inside a tasks.prime payload.
type TasksPrimePresence struct {
	AgentID   string             `json:"agent_id"`
	Project   string             `json:"project"`
	StartedAt timefmt.LoggedTime `json:"started_at"`
	LastSeen  timefmt.LoggedTime `json:"last_seen"`
}

// TasksReadyPayload is the payload for `bones tasks ready --json`.
// Always emits an array (never null); empty result yields `[]`.
type TasksReadyPayload []Task

// TasksShowPayload is the payload for `bones tasks show --json`.
// Single Task record.
type TasksShowPayload = Task

// TasksUpdatePayload is the payload for `bones tasks update --json`.
// Post-update Task record (or pre-update record when the update was
// a no-op).
type TasksUpdatePayload = Task

// WorkspacesGetPayload is the payload for `bones workspaces show <name> --json`.
// Single registry row.
type WorkspacesGetPayload = WorkspacesRow

// WorkspacesListPayload is the payload for `bones workspaces ls --json`.
// All registry rows in the deterministic sort order ListInfo applies.
type WorkspacesListPayload []WorkspacesRow

// WorkspacesRow is one registry entry as serialized by both
// `workspaces.list` and `workspaces.get`.
type WorkspacesRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Cwd         string `json:"cwd"`
	HubStatus   string `json:"hub_status"`
	LastTouched string `json:"last_touched"`
	AgentID     string `json:"agent_id"`
	NATSURL     string `json:"nats_url"`
	HubURL      string `json:"hub_url"`
}
