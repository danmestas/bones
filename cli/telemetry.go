package cli

import (
	"fmt"
	"os"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/telemetry"
)

// TelemetryCmd groups the operator-facing telemetry verbs introduced
// in ADR 0040. Default invocation (no subcommand) is treated as
// `status` per the ADR's "fewer subcommands to remember" decision.
//
// The verbs are file-system-only — no NATS, no fossil, no workspace.
// They work in any directory and on a fresh machine. Operators
// running `bones telemetry disable` immediately after install (before
// `bones up`) is the load-bearing UX.
type TelemetryCmd struct {
	Status  TelemetryStatusCmd  `cmd:"" default:"withargs" help:"Print current telemetry state"`
	Disable TelemetryDisableCmd `cmd:"" help:"Opt out of telemetry (writes ~/.bones/no-telemetry)"`
	Enable  TelemetryEnableCmd  `cmd:"" help:"Re-enable telemetry (removes the opt-out marker)"`
}

// TelemetryStatusCmd prints the current resolution outcome. Output
// shape is one labeled line per fact (state, reason, endpoint,
// dataset, install_id) so a script can parse it with grep.
type TelemetryStatusCmd struct{}

// Run renders the status. Always exits 0 — even "off" is a valid
// state to report.
func (c *TelemetryStatusCmd) Run(g *repocli.Globals) error {
	state := "off"
	if telemetry.IsEnabled() {
		state = "on"
	}
	_, _ = fmt.Fprintf(os.Stdout, "state:      %s\n", state)
	_, _ = fmt.Fprintf(os.Stdout, "reason:     %s\n", telemetry.StatusReason())
	if ep := telemetry.Endpoint(); ep != "" {
		_, _ = fmt.Fprintf(os.Stdout, "endpoint:   %s\n", ep)
	}
	if ds := telemetry.Dataset(); ds != "" {
		_, _ = fmt.Fprintf(os.Stdout, "dataset:    %s\n", ds)
	}
	if id := telemetry.InstallID(); id != "" {
		_, _ = fmt.Fprintf(os.Stdout, "install_id: %s\n", id)
	}
	if path := telemetry.OptOutPath(); path != "" {
		_, _ = fmt.Fprintf(os.Stdout, "opt_out:    %s\n", path)
	}
	return nil
}

// TelemetryDisableCmd writes the opt-out marker. Idempotent — safe
// to run on a fresh install or repeatedly. Prints a confirmation
// line so the operator sees the action took.
type TelemetryDisableCmd struct{}

// Run writes the marker file. Surfaces I/O errors directly with the
// path so the operator can investigate (e.g. permission issues on
// read-only home directories).
func (c *TelemetryDisableCmd) Run(g *repocli.Globals) error {
	if err := telemetry.Disable(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout,
		"telemetry disabled. Marker: %s\nRe-enable with: bones telemetry enable\n",
		telemetry.OptOutPath(),
	)
	return nil
}

// TelemetryEnableCmd removes the opt-out marker. Idempotent on a
// fresh install (no marker → no-op).
type TelemetryEnableCmd struct{}

// Run removes the marker. The follow-up resolution outcome depends
// on the build flavor and env vars; the printed hint points the
// operator at `bones telemetry status` rather than guessing.
func (c *TelemetryEnableCmd) Run(g *repocli.Globals) error {
	if err := telemetry.Enable(); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout,
		"telemetry opt-out marker removed. Run `bones telemetry status` to see resolved state.")
	return nil
}
