package registry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HubStatus is a coarse liveness label produced by ListInfo. Values:
//
//	HubRunning  — registry recorded a hub PID, the PID is alive on this host,
//	              and a quick TCP/HTTP probe of HubURL returned a response.
//	HubStopped  — registry has an entry, but the hub is not reachable
//	              (PID dead OR HTTP probe failed).
//	HubUnknown  — the entry has no enough info to probe (e.g. missing
//	              HubURL/HubPID), or a probe was deliberately skipped.
type HubStatus string

const (
	HubRunning HubStatus = "running"
	HubStopped HubStatus = "stopped"
	HubUnknown HubStatus = "unknown"
)

// Info is one workspace registry record enriched with the on-disk filename
// (ID), file mtime (LastTouched), the workspace's agent.id marker (when
// present), and a coarse hub liveness label.
//
// ID is the hex string used as the registry filename:
// ~/.bones/workspaces/<ID>.json. It equals WorkspaceID(Cwd).
type Info struct {
	Entry

	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id"`
	HubStatus   HubStatus `json:"hub_status"`
	LastTouched time.Time `json:"last_touched"`
}

// ListInfo enumerates every registry entry, attaching ID + mtime + agent.id +
// hub-status. Corrupt or unreadable files are skipped (matching List). The
// hub-status probe runs IsAlive on each entry; callers that want to skip
// the probe (e.g. for fast paths) should use List + their own enrichment.
//
// Self-prunes stale entries on read (#229) — same predicate as List(): a
// dead HubPID or a missing Cwd both qualify the entry as crud and the
// file is removed before this function returns.
//
// Results are sorted by Name, then Cwd, for stable output across calls.
func ListInfo() ([]Info, error) {
	paths, entries, err := pruneStale()
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(entries))
	for i, e := range entries {
		path := paths[i]
		fi, err := os.Stat(path)
		if err != nil {
			// File vanished between prune and stat — skip rather
			// than fail the listing. Behavior matches the pre-#229
			// best-effort skip-on-stat-error path.
			continue
		}
		info := Info{
			Entry:       e,
			ID:          idFromFilename(path),
			AgentID:     readAgentIDFile(e.Cwd),
			HubStatus:   probeStatus(e),
			LastTouched: fi.ModTime(),
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Cwd < out[j].Cwd
	})
	return out, nil
}

// idFromFilename strips the ".json" suffix from the registry path
// and returns the workspace ID. Canonical filenames after #250 are
// <id>.json so the basename is the id directly. A leftover legacy
// per-pid file (`<id>-<pid>.json`) is migrated by Read on first
// access; if one is observed before migration, strip the trailing
// "-<pid>" so the surfaced ID still equals WorkspaceID(Cwd).
// WorkspaceID is 16 hex chars with no hyphens so the first hyphen
// is unambiguously the id/pid separator.
func idFromFilename(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	if idx := strings.Index(base, "-"); idx >= 0 {
		return base[:idx]
	}
	return base
}

// readAgentIDFile reads <cwd>/.bones/agent.id and trims trailing whitespace,
// or returns "" if the marker is missing or unreadable. The registry entry
// is the source of truth for cwd, so we recompute the marker path from it.
// Returns "" rather than an error to keep ListInfo best-effort: a workspace
// directory that has been deleted out from under the registry should still
// show up in `bones workspaces ls` so the operator can clean it up.
func readAgentIDFile(cwd string) string {
	if cwd == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(cwd, ".bones", "agent.id"))
	if err != nil {
		// Fall through; cwd may have moved or .bones may have been
		// scrubbed. Treat as unknown rather than failing the listing.
		return ""
	}
	return strings.TrimSpace(string(data))
}

// probeStatus maps Entry → HubStatus using the same liveness probe that
// `bones status --all` uses for live-only filtering. We do not parallelize
// here: HealthTimeout caps each probe at 500ms by default, which is fast
// enough for the ls path even with several workspaces. If that becomes a
// problem we can promote the probe to a goroutine pool.
//
// Entries lacking a HubURL or HubPID are reported HubUnknown — they
// pre-date the field or were written by a buggy hub. Either way, we
// don't have enough signal to call them running or stopped.
func probeStatus(e Entry) HubStatus {
	if e.HubURL == "" || e.HubPID == 0 {
		return HubUnknown
	}
	if IsAlive(e) {
		return HubRunning
	}
	return HubStopped
}
