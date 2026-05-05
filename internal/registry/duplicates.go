package registry

import "path/filepath"

// Duplicates returns every registry entry whose canonical Cwd matches
// the given workspace AND whose HubPID is alive on this host. When the
// length is 0 or 1, the workspace has no concurrent hubs (steady
// state). When the length is >= 2, the issue described in #208 is
// present: two `bones hub start` invocations are competing for the
// same workspace's URL files and fossil state.
//
// Sibling primitive of Orphans(): both walk the per-pid registry, both
// filter on PID liveness, both stay read-only. Doctor and status
// consume Duplicates and emit WARN-class output; reaping is left to
// the operator (the brief defers automatic kill of duplicates to a
// dedicated verb, mirroring ADR 0043's read-only-doctor doctrine).
//
// PID-alive is the only liveness signal — no HTTP probe — because the
// duplicate scenario typically includes one hub that lost the port
// race and is no longer serving HTTP. Such a hub is still holding
// fossil inodes and overwriting URL files; the operator needs to know
// about it even if HealthTimeout would mark it down.
func Duplicates(cwd string) ([]Entry, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	target := filepath.Clean(cwd)
	var out []Entry
	for _, e := range all {
		if filepath.Clean(e.Cwd) != target {
			continue
		}
		if !pidAlive(e.HubPID) {
			continue
		}
		out = append(out, e)
	}
	if len(out) < 2 {
		// Only one (or zero) live entries — not a duplicate.
		return nil, nil
	}
	return out, nil
}
