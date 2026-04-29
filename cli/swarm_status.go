package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// activeThresholdSec is the seconds-since-LastRenewed cutoff between
// "active" and "stale" sessions. Sessions younger than this are
// treated as alive; older ones are stale candidates for `bones doctor`
// to flag. Mirrors the 90s heartbeat-skew window used elsewhere in
// the codebase.
const activeThresholdSec = 90

// SwarmStatusCmd lists every active swarm session in the workspace.
// Output is derived purely from KV state — there is no per-process
// liveness probe because every swarm verb is a fresh CLI invocation
// whose pid dies at end of verb. LastRenewed is the canonical signal:
// active = renewed within activeThresholdSec, stale = older.
type SwarmStatusCmd struct {
	JSON bool `name:"json" help:"emit JSON"`
}

// statusRow is the per-session JSON shape emitted with --json. The
// human table renders the same fields. Pulled out so the JSON
// schema is explicit and stable for downstream consumers (`bones
// doctor`, future `bones swarm dispatch`).
type statusRow struct {
	Slot         string    `json:"slot"`
	TaskID       string    `json:"task_id"`
	AgentID      string    `json:"agent_id"`
	Host         string    `json:"host"`
	LeafPID      int       `json:"leaf_pid"`
	StartedAt    time.Time `json:"started_at"`
	LastRenewed  time.Time `json:"last_renewed"`
	State        string    `json:"state"`
	StaleSeconds int64     `json:"stale_seconds"`
}

func (c *SwarmStatusCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()
	return c.run(ctx, info)
}

func (c *SwarmStatusCmd) run(ctx context.Context, info workspace.Info) error {
	sess, closeSess, err := openSwarmSessions(ctx, info)
	if err != nil {
		return err
	}
	defer closeSess()
	sessions, err := sess.List(ctx)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	rows := buildStatusRows(sessions, host, timeNow())
	if c.JSON {
		return emitStatusJSON(rows)
	}
	return emitStatusTable(rows)
}

// buildStatusRows attaches a state label and a stale-seconds counter
// to each session. State derivation:
//   - "active"        — last_renewed within activeThresholdSec on this host
//   - "stale"         — older than activeThresholdSec on this host
//   - "remote"        — session lives on another host, recently renewed
//   - "remote-stale"  — session lives on another host, last renewal old
//
// Note: there is no "dead" state. The Phase 1 contract is "every swarm
// verb is its own CLI invocation," so the recorded pid is always
// dead by the time a sibling verb reads the record. LastRenewed is
// the canonical signal.
func buildStatusRows(sessions []swarm.Session, host string, now time.Time) []statusRow {
	out := make([]statusRow, 0, len(sessions))
	for _, s := range sessions {
		stale := int64(now.Sub(s.LastRenewed).Seconds())
		state := classifyState(s, host, stale)
		out = append(out, statusRow{
			Slot:         s.Slot,
			TaskID:       s.TaskID,
			AgentID:      s.AgentID,
			Host:         s.Host,
			LeafPID:      s.LeafPID,
			StartedAt:    s.StartedAt,
			LastRenewed:  s.LastRenewed,
			State:        state,
			StaleSeconds: stale,
		})
	}
	return out
}

func classifyState(s swarm.Session, host string, staleSec int64) string {
	if s.Host != host {
		if staleSec > activeThresholdSec {
			return "remote-stale"
		}
		return "remote"
	}
	if staleSec > activeThresholdSec {
		return "stale"
	}
	return "active"
}

func emitStatusJSON(rows []statusRow) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

func emitStatusTable(rows []statusRow) error {
	if len(rows) == 0 {
		fmt.Println("(no active swarm sessions)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "SLOT\tTASK-ID\tHOST\tPID\tSTATE\tRENEWED"); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			r.Slot,
			truncateID(r.TaskID, 8),
			r.Host,
			r.LeafPID,
			r.State,
			fmt.Sprintf("%ds ago", r.StaleSeconds),
		); err != nil {
			return err
		}
	}
	return w.Flush()
}

// truncateID shortens long task UUIDs in the table view. The full
// IDs surface in --json output for downstream tools.
func truncateID(id string, n int) string {
	if len(id) <= n {
		return id
	}
	return strings.SplitN(id, "-", 2)[0]
}
