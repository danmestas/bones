package cli

import (
	"context"
	"fmt"
	"os"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// SwarmCwdCmd prints the slot's worktree path on stdout. Designed for
// shell substitution:
//
//	cd "$(bones swarm cwd --slot=rendering)"
//
// Pure path derivation — no NATS round-trip, no KV lookup. Only
// requires a workspace context to compute the absolute path. Callers
// who want the wt path AND a liveness check should use `swarm status`.
type SwarmCwdCmd struct {
	Slot string `name:"slot" required:"" help:"slot name"`
}

func (c *SwarmCwdCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	info, err := workspace.Join(context.Background(), cwd)
	if err != nil {
		return err
	}
	fmt.Println(swarm.SlotWorktree(info.WorkspaceDir, c.Slot))
	return nil
}
