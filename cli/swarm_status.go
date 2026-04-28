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

// SwarmStatusCmd lists every active swarm session in the workspace.
// Output combines KV state with a host-local PID liveness probe so a
// crashed leaf surfaces as DEAD even before the bucket TTL evicts it.
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
	mgr, closeMgr, err := openSwarmManager(ctx, info)
	if err != nil {
		return err
	}
	defer closeMgr()
	sessions, err := mgr.List(ctx)
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
//   - "active" — last_renewed within 90s of now AND (cross-host OR pid alive)
//   - "stale"  — older renewal but not yet TTL'd
//   - "dead"   — pid dead on this host
//   - "remote" — session lives on another host
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
	const activeThreshold = 90 // seconds since last renewal
	if s.Host != host {
		if staleSec > activeThreshold {
			return "remote-stale"
		}
		return "remote"
	}
	if s.LeafPID > 0 && !pidAlive(s.LeafPID) {
		return "dead"
	}
	if staleSec > activeThreshold {
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
	fmt.Fprintln(w, "SLOT\tTASK-ID\tHOST\tPID\tSTATE\tRENEWED")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			r.Slot,
			truncateID(r.TaskID, 8),
			r.Host,
			r.LeafPID,
			r.State,
			fmt.Sprintf("%ds ago", r.StaleSeconds),
		)
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
