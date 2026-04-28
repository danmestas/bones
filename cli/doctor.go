package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	edgecli "github.com/danmestas/EdgeSync/cli"
	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// DoctorCmd extends EdgeSync's doctor with bones-specific checks. The
// embedded EdgeSync DoctorCmd runs the base health gate (Go runtime,
// fossil, NATS reachability, hooks); then this wrapper adds the
// swarm-session inventory described in ADR 0028 §"Process lifecycle
// and crash recovery" so stuck or cross-host slots surface here.
//
// Embedded — not aliased — so EdgeSync's flags (--nats-url) still
// participate in Kong parsing.
type DoctorCmd struct {
	edgecli.DoctorCmd
}

// Run invokes the EdgeSync doctor first; on completion (regardless
// of pass/warn/fail) it appends a "swarm sessions" section that
// iterates bones-swarm-sessions and reports each entry's state.
func (c *DoctorCmd) Run(g *libfossilcli.Globals) error {
	// The EdgeSync side returns an error on failed checks; surface
	// that error AFTER our additional report so operators see the
	// swarm picture even when an upstream check failed.
	baseErr := c.DoctorCmd.Run(g)
	swarmErr := c.runSwarmReport()
	if baseErr != nil {
		return baseErr
	}
	return swarmErr
}

// runSwarmReport prints the swarm-session inventory or a brief
// "(no workspace)" line when the cwd is not inside a bones workspace.
// Errors connecting to NATS surface as warnings rather than fail
// the whole doctor — `bones doctor` is meant to be informational
// even on a half-broken setup.
func (c *DoctorCmd) runSwarmReport() error {
	fmt.Println()
	fmt.Println("=== bones swarm sessions ===")
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("  WARN  cwd: %v\n", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := workspace.Join(ctx, cwd)
	if err != nil {
		fmt.Printf("  INFO  not in a bones workspace (%v)\n", err)
		return nil
	}
	mgr, closer, err := openSwarmManager(ctx, info)
	if err != nil {
		fmt.Printf("  WARN  open swarm manager: %v\n", err)
		return nil
	}
	defer closer()
	sessions, err := mgr.List(ctx)
	if err != nil {
		fmt.Printf("  WARN  list sessions: %v\n", err)
		return nil
	}
	if len(sessions) == 0 {
		fmt.Println("  OK    no active swarm sessions")
		return nil
	}
	host, _ := os.Hostname()
	stale := 0
	remote := 0
	for _, s := range sessions {
		state := classifySwarmSession(s, host)
		if state == "stale" || state == "remote-stale" {
			stale++
		}
		if state == "remote" {
			remote++
		}
		fmt.Printf("  %-6s  slot=%-12s task=%s host=%s\n",
			labelFor(state), s.Slot, truncateID(s.TaskID, 8), s.Host)
	}
	if stale+remote == 0 {
		fmt.Printf("  OK    %d active session(s)\n", len(sessions))
	} else {
		fmt.Printf("  NOTE  %d active, %d remote, %d stale\n",
			len(sessions)-stale-remote, remote, stale)
	}
	return nil
}

// classifySwarmSession reuses the same state model swarm status uses
// (the function lives in cli/swarm_status.go) but presented for
// doctor output. Indirection keeps both consumers symmetric.
func classifySwarmSession(s swarm.Session, host string) string {
	staleSec := int64(time.Since(s.LastRenewed).Seconds())
	return classifyState(s, host, staleSec)
}

func labelFor(state string) string {
	switch state {
	case "active":
		return "OK"
	case "remote":
		return "OK"
	case "stale", "remote-stale":
		return "WARN"
	}
	return "INFO"
}
