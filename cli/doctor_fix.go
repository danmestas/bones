package cli

import (
	"fmt"
	"io"
)

// printFix renders a single recovery-command hint indented under a doctor
// finding. Format: "        Fix: <command>".
func printFix(w io.Writer, command string) {
	_, _ = fmt.Fprintf(w, "        Fix: %s\n", command)
}

// Closed catalog of fixes. Each finding type maps to one templated command.
// Adding a new finding requires extending this catalog explicitly — keeps
// hint logic in one place rather than scattered through individual checks.

// FixForStaleSlot returns the fix command for releasing a stale claim.
func FixForStaleSlot(slot string) string {
	return fmt.Sprintf("bones swarm close --slot=%s --result=fail", slot)
}

// FixForRemoteSlot returns an advisory hint for a remote-owned slot.
func FixForRemoteSlot(host string) string {
	return fmt.Sprintf("ssh %s 'bones swarm close --slot=<slot> --result=fail'", host)
}

// FixForMissingHook returns the fix command for a missing pre-commit hook.
func FixForMissingHook() string {
	return "bones up"
}

// FixForScaffoldDrift returns the fix command for scaffold version drift.
func FixForScaffoldDrift() string {
	return "bones up"
}

// FixForFossilDrift returns the fix command for fossil/git HEAD divergence.
// Per ADR 0037, `bones apply` materializes the fossil trunk tip into the
// git working tree; this is the correct path when fossil tip != git HEAD.
func FixForFossilDrift() string {
	return "bones apply"
}

// FixForHubDown returns an advisory hint for a workspace whose hub is not responding.
func FixForHubDown() string {
	return "bones up  # restarts hub for this workspace"
}

// FixForMissingFossil returns the fix command when fossil is absent from PATH.
func FixForMissingFossil() string {
	return "install fossil from https://fossil-scm.org/install"
}
