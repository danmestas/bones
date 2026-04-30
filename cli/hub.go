package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/scaffoldver"
	"github.com/danmestas/bones/internal/version"
)

// HubCmd is the umbrella command for the embedded Fossil + NATS hub.
//
// Subcommands:
//
//	hub start [--detach]    bring the hub up
//	hub stop                tear it down
//	hub user add <login>    pre-create a fossil user in the hub repo
//	hub user list           list fossil users in the hub repo
//
// The shipped scripts under .orchestrator/scripts/ are thin shims around
// these subcommands, kept for backward compatibility with .claude/settings.json
// hooks generated before the Go-native hub.
type HubCmd struct {
	Start HubStartCmd `cmd:"" help:"Start the embedded Fossil hub + NATS server"`
	Stop  HubStopCmd  `cmd:"" help:"Stop the embedded Fossil hub + NATS server"`
	User  HubUserCmd  `cmd:"" help:"Manage fossil users in the hub repo"`
}

// HubStartCmd wires `bones hub start` flags to hub.Start.
//
// --detach (default false) is what shell hooks want: spawn a background
// hub and return immediately once both servers are reachable. Without
// --detach, the command runs the hub in the foreground and shuts both
// servers down on SIGINT/SIGTERM. Foreground mode is the easiest way to
// see hub logs interactively.
type HubStartCmd struct {
	Detach     bool `name:"detach" help:"return immediately after the hub is reachable"`
	FossilPort int  `name:"fossil-port" help:"Fossil HTTP port" default:"8765"`
	NATSPort   int  `name:"nats-port" help:"NATS client port" default:"4222"`
}

func (c *HubStartCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}

	warnScaffoldDrift(cwd)

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return hub.Start(ctx, cwd,
		hub.WithFossilPort(c.FossilPort),
		hub.WithNATSPort(c.NATSPort),
		hub.WithDetach(c.Detach),
	)
}

// warnScaffoldDrift prints a single-line stderr notice when the
// workspace scaffold version disagrees with the running binary's
// version. Best-effort: any read error is silent. Fires on every
// `bones hub start`, which the SessionStart hook runs at the
// beginning of each Claude Code session — so the operator sees it
// the moment they start working in a stale workspace.
func warnScaffoldDrift(cwd string) {
	stamp, err := scaffoldver.Read(cwd)
	if err != nil || !scaffoldver.Drifted(stamp, version.Get()) {
		return
	}
	fmt.Fprintf(os.Stderr,
		"bones: scaffold v%s, binary v%s — run `bones up` to refresh skills/hooks\n",
		stamp, version.Get())
}

// HubStopCmd wires `bones hub stop` to hub.Stop. Idempotent.
type HubStopCmd struct{}

func (c *HubStopCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	return hub.Stop(cwd)
}
