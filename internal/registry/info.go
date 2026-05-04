package registry

import (
	"encoding/json"
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
// Results are sorted by Name, then Cwd, for stable output across calls.
func ListInfo() ([]Info, error) {
	matches, err := filepath.Glob(filepath.Join(RegistryDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(matches))
	for _, path := range matches {
		// Skip atomic-write tmp files (".tmp.*"); only top-level *.json
		// files are real entries.
		if strings.Contains(filepath.Base(path), ".tmp.") {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		// Read the entry payload via the canonical Read seam so any
		// future migrations land here too.
		e, err := readEntryAtPath(path)
		if err != nil {
			continue
		}
		info := Info{
			Entry:       e,
			ID:          strings.TrimSuffix(filepath.Base(path), ".json"),
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

// readEntryAtPath is the path-keyed sibling of Read. Read is cwd-keyed and
// recomputes the filename via WorkspaceID; ListInfo already has the path,
// so we read it directly to avoid the extra hash + to surface decode
// errors at the right path.
func readEntryAtPath(path string) (Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return Entry{}, err
	}
	return e, nil
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
