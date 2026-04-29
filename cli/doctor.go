package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	edgecli "github.com/danmestas/EdgeSync/cli"
	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/githook"
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
	c.runBypassReport()
	if baseErr != nil {
		return baseErr
	}
	return swarmErr
}

// runBypassReport prints the ADR-0034 bypass-prevention checks: is
// the pre-commit hook installed, and does the trunk fossil tip agree
// with git HEAD. Both are best-effort — failures here do not fail
// doctor, they surface as WARN so the operator sees the drift before
// it bites.
func (c *DoctorCmd) runBypassReport() {
	fmt.Println()
	fmt.Println("=== bones substrate gates (ADR 0034) ===")

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("  WARN  cwd: %v\n", err)
		return
	}

	gitDir := githook.FindGitDir(cwd)
	if gitDir == "" {
		fmt.Println("  INFO  no .git found — skipping hook check")
	} else {
		installed, err := githook.IsInstalled(gitDir)
		switch {
		case err != nil:
			fmt.Printf("  WARN  hook read failed: %v\n", err)
		case !installed:
			fmt.Println("  WARN  pre-commit hook missing — run `bones up` to reinstall")
		default:
			fmt.Println("  OK    pre-commit hook installed")
		}
	}

	switch tip, head, drifted := fossilDrift(cwd); {
	case tip == "" && head == "":
		fmt.Println("  INFO  no git or fossil state to compare")
	case tip == "":
		fmt.Println("  INFO  trunk fossil empty — first commit will seed it")
	case head == "":
		fmt.Println("  WARN  cannot read git HEAD — is this a git workspace?")
	case drifted:
		fmt.Printf("  WARN  fossil tip (%s) != git HEAD (%s) — re-init bones or apply pending\n",
			short(tip), short(head))
	default:
		fmt.Println("  OK    fossil tip == git HEAD")
	}
}

// fossilDrift reads the fossil trunk tip marker and git HEAD; it
// returns both values plus a drifted flag. Empty strings mean the
// respective side could not be read; the caller decides how to
// classify each combination.
func fossilDrift(cwd string) (tip, head string, drifted bool) {
	tip = readTrunkTip(cwd)
	head = gitHead(cwd)
	if tip != "" && head != "" && tip != head {
		drifted = true
	}
	return
}

func readTrunkTip(cwd string) string {
	path := filepath.Join(cwd, ".bones", "trunk_tip")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func gitHead(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
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
	sess, closer, err := openSwarmSessions(ctx, info)
	if err != nil {
		fmt.Printf("  WARN  open swarm sessions: %v\n", err)
		return nil
	}
	defer closer()
	sessions, err := sess.List(ctx)
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
