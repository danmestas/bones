package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/scaffoldver"
	"github.com/danmestas/bones/internal/swarm"
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
// Per ADR 0041 these subcommands are the only entry points to hub
// lifecycle: the legacy bash bootstrap shims under .orchestrator/scripts/
// are no longer scaffolded.
type HubCmd struct {
	Start HubStartCmd `cmd:"" help:"Start the embedded Fossil hub + NATS server"`
	Stop  HubStopCmd  `cmd:"" help:"Stop the embedded Fossil hub + NATS server"`
	User  HubUserCmd  `cmd:"" help:"Manage fossil users in the hub repo"`
	Reap  HubReapCmd  `cmd:"" help:"Terminate orphan hub processes (ADR 0043)"`
}

// HubStartCmd wires `bones hub start` flags to hub.Start.
//
// --detach (default false) is what shell hooks want: spawn a background
// hub and return immediately once both servers are reachable. Without
// --detach, the command runs the hub in the foreground and shuts both
// servers down on SIGINT/SIGTERM. Foreground mode is the easiest way to
// see hub logs interactively.
type HubStartCmd struct {
	Detach bool `name:"detach" help:"return immediately after the hub is reachable"`
	// 0 = let the hub allocate per-workspace (default). The hub records
	// the resolved URL at .bones/hub-{fossil,nats}-url so a
	// second workspace can run concurrently on its own free ports.
	// Pass an explicit non-zero port to pin.
	RepoPort  int `name:"repo-port" default:"0" help:"repo HTTP port (0 = per-ws)"`
	CoordPort int `name:"coord-port" default:"0" help:"coord client port (0 = per-ws)"`
	// DrainTimeout bounds NATS/Fossil drain on shutdown before
	// runForeground returns errDrainTimeout (non-zero exit). See #158.
	DrainTimeout time.Duration `name:"drain-timeout" default:"30s" help:"max drain wait"` //nolint:lll
}

func (c *HubStartCmd) Run(g *repocli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}

	warnScaffoldDrift(cwd)

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return hub.Start(ctx, cwd,
		hub.WithRepoPort(c.RepoPort),
		hub.WithCoordPort(c.CoordPort),
		hub.WithDetach(c.Detach),
		hub.WithDrainTimeout(c.DrainTimeout),
		hub.WithLeaseWatcher(startLeaseWatcher),
	)
}

// startLeaseWatcher is the CLI-side lease-TTL watcher constructor
// the hub invokes once both servers are reachable (ADR 0050,
// #265). Lives here (not in internal/hub) because it imports
// swarm; putting it inside the hub package would create a
// hub→swarm→workspace→hub import cycle.
//
// Best-effort construction: NATS dial / sessions open failures are
// surfaced as errors to the hub, which logs them as warnings and
// continues serving. The JetStream KV bucket TTL still evicts
// stale records at the substrate level even without this watcher.
func startLeaseWatcher(
	parentCtx context.Context, info hub.LeaseWatcherInfo,
) (func(), error) {
	nc, err := nats.Connect(info.NATSURL)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	sess, err := swarm.Open(parentCtx, swarm.Config{NATSConn: nc})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("open sessions: %w", err)
	}
	w, err := swarm.NewTTLWatcher(swarm.WatcherConfig{
		WorkspaceDir:  info.WorkspaceDir,
		Sessions:      sess,
		Logger:        leaseWatcherLogger{info.Logger},
		LocalHostOnly: true,
	})
	if err != nil {
		_ = sess.Close()
		nc.Close()
		return nil, fmt.Errorf("new watcher: %w", err)
	}
	stopRun := w.Start(parentCtx)
	return func() {
		stopRun()
		_ = sess.Close()
		nc.Close()
	}, nil
}

// leaseWatcherLogger adapts the hub's LeaseWatcherLogger to
// swarm.WatcherLogger. The two types share a method set; the
// adapter is a one-line shape conversion so neither package needs
// to import the other's logger interface directly.
type leaseWatcherLogger struct{ inner hub.LeaseWatcherLogger }

func (a leaseWatcherLogger) Infof(format string, args ...any) {
	a.inner.Infof(format, args...)
}

func (a leaseWatcherLogger) Warnf(format string, args ...any) {
	a.inner.Warnf(format, args...)
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
//
// --force overrides the active-slot safety check (#157). Without it,
// stop refuses when any .bones/swarm/<slot>/leaf.pid points at a live
// process, since restarting the hub on a different port would silently
// break those leaves' cached NATS URLs.
type HubStopCmd struct {
	Force bool `name:"force" help:"stop even when swarm slots are active (#157)"`
}

func (c *HubStopCmd) Run(g *repocli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	return hub.Stop(cwd, hub.WithForce(c.Force))
}
