package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/workspace"
)

// peekPortRangeStart is where the auto-port scanner begins looking
// for a free local port when --port is not specified. Sits 10 above
// the hub's Fossil port (8765) to stay grouped with the bones port
// range, well above the common 3000/8080 dev-server cluster.
const peekPortRangeStart = 8775

// peekPortRangeEnd caps the scan. If nothing in the preferred range
// is free, peek falls back to letting fossil pick its own default;
// the user can always override with --port.
const peekPortRangeEnd = 8799

// PeekCmd opens the workspace's hub Fossil repo in the system fossil
// binary's web UI (timeline, branches, files). It is an *enhancement*,
// not a hard dependency: when `fossil` isn't on PATH, peek prints the
// hub repo path with a one-line install hint and exits cleanly.
//
// libfossil's embedded HTTP server (used by `bones hub start` to serve
// /xfer for sync) does not implement the rich Fossil web UI; peek
// shells out to the canonical `fossil ui` for that.
type PeekCmd struct {
	Port int    `name:"port" help:"bind the UI on this port (default: first free in 8775-8799)"`
	Page string `name:"page" default:"timeline?y=ci&n=50" help:"fossil page (e.g. timeline)"`
}

func (c *PeekCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	info, err := workspace.Join(context.Background(), cwd)
	if err != nil {
		return fmt.Errorf("workspace: %w (run `bones init` first)", err)
	}

	hubRepo := hub.HubFossilPath(info.WorkspaceDir)
	if _, err := os.Stat(hubRepo); err != nil {
		return fmt.Errorf(
			"hub repo not found at %s — run `bones up` or `bones hub start` first",
			hubRepo,
		)
	}

	fossilBin, lookErr := exec.LookPath("fossil")
	if lookErr != nil {
		fmt.Printf("peek: install fossil to open the rich timeline UI:\n")
		fmt.Printf("  brew install fossil   # macOS / Linux (homebrew)\n")
		fmt.Printf("  apt install fossil    # Debian / Ubuntu\n")
		fmt.Printf("\n")
		fmt.Printf("hub repo: %s\n", hubRepo)
		fmt.Printf("(any Fossil-compatible tool can open this file directly)\n")
		return nil
	}

	port := c.Port
	if port == 0 {
		port = pickPeekPort()
	}

	args := []string{"ui", hubRepo}
	if port > 0 {
		args = append(args, "--port", strconv.Itoa(port))
	}
	if c.Page != "" {
		args = append(args, "--page", c.Page)
	}

	if port > 0 {
		fmt.Printf("peek: %s ui %s --port %d\n", fossilBin, hubRepo, port)
	} else {
		fmt.Printf("peek: %s ui %s\n", fossilBin, hubRepo)
	}
	cmd := exec.Command(fossilBin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// pickPeekPort scans peekPortRangeStart through peekPortRangeEnd and
// returns the first port that accepts a local listener. Returns 0
// when none are free; the caller falls back to fossil's own default.
//
// The probe binds + immediately closes; there's a small window
// between this call and fossil's bind where a competing process
// could grab the port. That's a tradeoff: deterministic-port output
// for the user, vs. a race that fossil's own bind would surface as
// a clear error anyway.
func pickPeekPort() int {
	for p := peekPortRangeStart; p <= peekPortRangeEnd; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		_ = l.Close()
		return p
	}
	return 0
}
