package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/hub"
)

// HubCmd is the umbrella command for the embedded Fossil + NATS hub.
//
// Subcommands:
//
//	hub start [--detach]   bring the hub up
//	hub stop               tear it down
//
// The shipped scripts under .orchestrator/scripts/ are thin shims around
// these subcommands, kept for backward compatibility with .claude/settings.json
// hooks generated before the Go-native hub.
type HubCmd struct {
	Start HubStartCmd `cmd:"" help:"Start the embedded Fossil hub + NATS server"`
	Stop  HubStopCmd  `cmd:"" help:"Stop the embedded Fossil hub + NATS server"`
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

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return hub.Start(ctx, cwd,
		hub.WithFossilPort(c.FossilPort),
		hub.WithNATSPort(c.NATSPort),
		hub.WithDetach(c.Detach),
	)
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
