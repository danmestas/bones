package hub

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	edgehub "github.com/danmestas/EdgeSync/hub"

	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/slotgc"
	"github.com/danmestas/bones/internal/telemetry"
)

// acquireWorkspaceLockFunc is a seam over registry.AcquireWorkspaceLock
// so tests can substitute a no-op or contention stub without spawning
// real subprocesses. Production callers reach the real flock-based
// implementation.
var acquireWorkspaceLockFunc = registry.AcquireWorkspaceLock

// detachEnv, when set, signals to a fork-exec'd child process that it
// should run Start in foreground regardless of the --detach flag. The
// CLI sets this on the child to break the recursion.
const detachEnv = "BONES_HUB_FOREGROUND"

// markerDirName is the workspace-local directory housing all hub state.
// Aligned with internal/workspace's markerDirName per ADR 0041 (collapsed
// from the legacy .orchestrator/ + .bones/ split into a single .bones/).
const markerDirName = ".bones"

// readyTimeout bounds how long Start waits for each server to accept
// connections. ADR 0034 raised this from 5s to 15s after the
// 2026-04-29 serverdom incident, where loaded-machine NATS startup
// reliably exceeded 5s on first attempt and was misread by operators
// as bones being broken.
const readyTimeout = 15 * time.Second

// pollInterval is how often readiness probes retry. Short enough to keep
// total wakeup latency low, long enough not to busy-spin on the listener.
const pollInterval = 25 * time.Millisecond

// natsBootstrapAttempts is the number of times startNATS retries on
// readiness failure before giving up. Each retry uses readyTimeout
// for its own probe; backoff is exponential between attempts.
const natsBootstrapAttempts = 3

// natsBootstrapBackoff is the wait between failed attempts. Doubles
// each round (1s, 2s, 4s).
const natsBootstrapBackoff = 1 * time.Second

// stopGrace bounds how long Stop waits between SIGTERM and SIGKILL.
// Mirrors registry.Reap: short by design, since hub processes hold
// ports + unlinked fossil inodes we want released ASAP. Long enough
// for a healthy hub to flush WAL and exit cleanly on TERM.
var stopGrace = 2 * time.Second

// defaultDrainTimeout bounds the in-process NATS shutdown wait and the
// Fossil drain wait when ctx is canceled. Without a bound, a stuck
// leaf or fossil checkpoint can keep the hub process alive
// indefinitely — natsserver.Server.WaitForShutdown has no timeout
// (#158). 30s is generous for a healthy shutdown and short enough
// that operators do not wait minutes for force-kill escalation.
const defaultDrainTimeout = 30 * time.Second

// errDrainTimeout is returned from runForeground when the NATS or
// Fossil drain blocks past the configured drain timeout. Surfaces a
// non-zero exit code at the CLI so the parent can distinguish a clean
// shutdown from a forced one.
var errDrainTimeout = errors.New("hub: drain timeout exceeded")

// Start brings up the orchestrator hub: a Fossil repository at
// .bones/hub.fossil seeded from git-tracked files, a Fossil HTTP
// server on the chosen port, and an embedded NATS JetStream server.
//
// Idempotent: if hub.pid exists and the recorded process is alive,
// Start returns nil immediately.
//
// With WithDetach(true) the calling process fork-execs itself in
// "foreground" mode, waits for both servers to become reachable, and
// returns. The child outlives the caller and owns the servers;
// hub.pid references the child. This is what `bones hub start --detach`
// uses so a shell can fire-and-forget the hub.
//
// Without detach, Start blocks on ctx.Done(): the calling process is
// the hub. hub.pid references the calling process. On cancellation,
// both servers shut down cleanly and hub.pid is removed.
func Start(ctx context.Context, root string, options ...Option) (err error) {
	o := defaults()
	for _, fn := range options {
		fn(&o)
	}
	// Re-entry from the fork-exec'd child: even if the user passed
	// WithDetach(true), the BONES_HUB_FOREGROUND env var (set by our
	// fork) forces foreground so we don't recurse.
	isDetachChild := os.Getenv(detachEnv) == "1"
	if isDetachChild {
		o.detach = false
	}

	if mErr := gateStaleWorktrees(root, isDetachChild); mErr != nil {
		return mErr
	}
	p, err := newPaths(root)
	if err != nil {
		return err
	}

	var endTelemetry telemetry.EndFunc
	ctx, endTelemetry = startStartTelemetry(ctx, isDetachChild, p.fossilURL, o.detach)
	defer func() { endTelemetry(err) }()

	if err := os.MkdirAll(p.logDir, 0o755); err != nil {
		return fmt.Errorf("hub: logs dir: %w", err)
	}

	// One-time migration (#254): the legacy .bones/pids/ subdir held
	// fossil.pid and nats.pid for the same supervisor process. Remove
	// it on first Start after upgrade so stale pid files don't confuse
	// any external tooling that still scans the legacy path. Best-
	// effort: a permission failure here must not block hub start.
	_ = os.RemoveAll(filepath.Join(p.orchDir, "pids"))

	// Idempotency: if hub.pid points at a live process, we're done.
	if pidIsLive(p.hubPid) {
		return nil
	}

	// Workspace-scoped lock guard (#208 prevention layer). Acquire
	// BEFORE port resolution, URL-file writes, or fork-exec so a
	// second concurrent `bones hub start` against the same workspace
	// fails fast without side effects. Skip in the detached child:
	// the parent already gated the start.
	releaseLock, lockErr := acquireStartLock(p.root, isDetachChild)
	if lockErr != nil {
		return lockErr
	}
	defer releaseLock()

	// Seed precondition (#138 item 9): a fresh hub start needs at least
	// one git-tracked file to seed the hub fossil from. Without this
	// check the parent spawns a detached child, the child crashes inside
	// seedHubRepo with "no git-tracked files to seed from", and the
	// parent waits the full readyTimeout (15s) for a TCP probe that will
	// never succeed before surfacing the real error from hub.log. Skip
	// the check on detach re-entry: the parent already ran it.
	if !isDetachChild {
		needsSeed := false
		if _, statErr := os.Stat(p.hubRepo); errors.Is(statErr, os.ErrNotExist) {
			needsSeed = true
		}
		if needsSeed {
			if err := checkSeedPrecondition(p.root); err != nil {
				return err
			}
		}
	}

	// Per-slot GC: remove .bones/swarm/<slot>/ directories whose
	// leaf.pid points at a dead process. Piggybacks on the most
	// frequently-run lifecycle event (SessionStart hook → bones hub
	// start) so stale slot dirs don't accumulate across sessions
	// (#130). Best-effort: a permission failure on one slot doesn't
	// block hub start for the rest.
	if pruned, err := slotgc.PruneDead(p.root); err != nil {
		fmt.Fprintf(os.Stderr,
			"hub: swarm gc warning (non-fatal): %v\n", err)
	} else if len(pruned) > 0 {
		fmt.Fprintf(os.Stderr,
			"hub: pruned %d stale slot dir(s): %s\n",
			len(pruned), strings.Join(pruned, ", "))
	}

	// Resolve any zero-valued port to the workspace's recorded port
	// (recovery / restart) or a fresh free one (first run, or after
	// `bones down`). Writes the URL files so consumers can discover the
	// live hub.
	if err := resolvePorts(p, &o); err != nil {
		return err
	}

	if o.detach {
		return spawnDetachedChild(p, o)
	}
	return runForeground(ctx, p, o)
}

// runForeground executes the full bring-up in the calling process and
// blocks on ctx.Done(). Pid files name the current process so other
// shells can detect liveness, and Stop in this same process is
// equivalent to canceling ctx.
func runForeground(ctx context.Context, p paths, o opts) error {
	// Open the lifecycle log first so `hub: starting` is the first
	// thing we write — diagnosing a crashed-hub workspace begins with
	// hub.log, and every event after this point lands here too (#247).
	hl := openHubLog(p)
	defer hl.Close()
	hl.Infof("hub: starting (pid=%d, repo-port=%d, coord-port=%d)",
		os.Getpid(), o.repoPort, o.coordPort)

	// Fresh-start detection (ADR 0023): if hub.pid is not alive,
	// wipe stale checkout state so each session starts from a clean
	// substrate. Working-tree files are untouched. SQLite -shm and -wal
	// sidecars are part of stale state — without removing them, the
	// next bootstrap hits SQLITE_IOERR_SHORT_READ (522). See #138.
	if !pidIsLive(p.hubPid) {
		removeRepoAndSidecars(p)
	}
	if err := os.MkdirAll(p.coordStore, 0o755); err != nil {
		hl.Errorf("hub: failed to create coord store dir: %v", err)
		return fmt.Errorf("hub: coord store dir: %w", err)
	}
	// Write hub.pid BEFORE NewHub binds the HTTP listener. edgehub.NewHub
	// calls net.Listen at construction (the HTTP socket is up before
	// ServeHTTP runs), and the kernel SYN-ACKs connections to a bound-but-
	// unaccepted socket — so the detach parent's waitForTCP probe succeeds
	// the moment NewHub returns. If we wrote hub.pid after NewHub, the
	// parent's port-collision check (readPid against cmd.Process.Pid)
	// would race ahead of the pid write and fire a false-positive
	// "fossil port responded but hub.pid does not name our child
	// (recorded 0)" error on every detach start. The pid file names the
	// foreground process either way (os.Getpid() is stable across the
	// NewHub call), so writing it first is correct.
	if err := writePid(p.hubPid, os.Getpid()); err != nil {
		hl.Errorf("hub: write hub.pid failed: %v", err)
		return fmt.Errorf("hub: write hub.pid: %w", err)
	}
	freshSeed := false
	if _, err := os.Stat(p.hubRepo); errors.Is(err, os.ErrNotExist) {
		freshSeed = true
	}
	h, err := openAndSeedHub(ctx, p, o, freshSeed)
	if err != nil {
		_ = os.Remove(p.hubPid)
		hl.Errorf("hub: bring-up failed: %v", err)
		return err
	}
	httpDone := make(chan struct{})
	httpCtx, httpCancel := context.WithCancel(context.Background())
	go func() {
		defer close(httpDone)
		_ = h.ServeHTTP(httpCtx)
	}()
	fmt.Printf("hub: fossil at %s, nats at %s\n", h.HTTPAddr(), h.NATSURL())
	if err := registry.Write(registry.Entry{
		Cwd:       p.root,
		Name:      filepath.Base(p.root),
		HubURL:    "http://" + h.HTTPAddr(),
		NATSURL:   h.NATSURL(),
		HubPID:    os.Getpid(),
		StartedAt: time.Now().UTC(),
	}); err != nil {
		hl.Warnf("hub: registry write failed (non-fatal): %v", err)
		fmt.Fprintf(os.Stderr, "hub: registry write failed (non-fatal): %v\n", err)
	}
	hl.Infof("hub: ready")

	stopWatcher := startLeaseWatcherHook(ctx, o, p, h.NATSURL(), hl)
	defer stopWatcher()

	<-ctx.Done()
	hl.Infof("hub: stopping")
	httpCancel()
	drainErr := drainHub(h, httpDone, o.drainTimeout)
	_ = os.Remove(p.hubPid)
	// Drop our own registry entry so a clean hub exit does not leave
	// a stale record for `bones doctor` or `bones status --all` to
	// filter out via pidAlive. Dead-pid pruning still handles the
	// unclean-exit case. Single-file layout since #250.
	_ = registry.Remove(p.root)
	if drainErr != nil {
		hl.Errorf("hub: stopped with drain error: %v", drainErr)
	} else {
		hl.Infof("hub: stopped")
	}
	return drainErr
}

// startLeaseWatcherHook runs the optional lease-TTL watcher (ADR
// 0050 / #265) if the caller provided a start function via
// WithLeaseWatcher. Pulled out of runForeground so that function
// stays under the funlen lint cap; the option indirection keeps
// the hub package free of the swarm import (avoids the
// hub→swarm→workspace→hub cycle).
//
// Returns a stop function the caller defers; a no-op when no
// watcher hook was supplied or when the start callback returned
// an error (which is logged as a warning to hub.log).
func startLeaseWatcherHook(
	ctx context.Context, o opts, p paths, natsURL string, hl *hubLogger,
) func() {
	if o.startLeaseWatcher == nil {
		return func() {}
	}
	stop, err := o.startLeaseWatcher(ctx, LeaseWatcherInfo{
		WorkspaceDir: p.root,
		NATSURL:      natsURL,
		Logger:       hubLogAdapter{l: hl},
	})
	if err != nil {
		hl.Warnf("hub: lease watcher: %v (watcher disabled)", err)
		return func() {}
	}
	return stop
}

// hubLogAdapter exposes a small subset of hubLogger as a public
// shape consumable by the lease-watcher hook. Defined as a struct
// (rather than an exported interface) so the watcher's function
// signature stays a value-type contract — no behavior in this
// package is overrideable from outside.
type hubLogAdapter struct{ l *hubLogger }

// Infof routes one INFO-level lifecycle event into hub.log.
func (a hubLogAdapter) Infof(format string, args ...any) {
	a.l.Infof(format, args...)
}

// Warnf routes one WARN-level lifecycle event into hub.log.
func (a hubLogAdapter) Warnf(format string, args ...any) {
	a.l.Warnf(format, args...)
}

// openAndSeedHub brings up the EdgeSync hub and seeds the repo if
// freshSeed is true. On any failure the partial state is rolled back.
func openAndSeedHub(
	ctx context.Context, p paths, o opts, freshSeed bool,
) (*edgehub.Hub, error) {
	h, err := edgehub.NewHub(ctx, edgehub.Config{
		RepoPath:       p.hubRepo,
		BootstrapUser:  "orchestrator",
		NATSStoreDir:   p.coordStore,
		FossilHTTPPort: o.repoPort,
		NATSClientPort: o.coordPort,
	})
	if err != nil {
		removeRepoAndSidecars(p)
		return nil, fmt.Errorf("hub: %w", err)
	}
	if freshSeed {
		if err := seedHubRepoFunc(ctx, h, p); err != nil {
			_ = h.Stop()
			removeRepoAndSidecars(p)
			return nil, fmt.Errorf("hub: seed: %w", err)
		}
	}
	return h, nil
}

// drainHub stops the hub and waits for ServeHTTP to exit, bounded by
// drainTimeout. On timeout, abandons the wait, logs the forced exit,
// and returns errDrainTimeout so the CLI exits non-zero (#158).
func drainHub(h *edgehub.Hub, httpDone <-chan struct{}, drainTimeout time.Duration) error {
	hubDone := make(chan struct{})
	go func() {
		_ = h.Stop()
		<-httpDone
		close(hubDone)
	}()
	if err := waitOrTimeout(hubDone, drainTimeout); err != nil {
		fmt.Fprintf(os.Stderr,
			"hub: shutdown exceeded %s; forcing exit (#158)\n",
			effectiveDrainTimeout(drainTimeout))
		return err
	}
	return nil
}

// effectiveDrainTimeout returns d when positive, otherwise the package
// default. Used so both runForeground's stderr lines and the actual
// wait honor the same fallback.
func effectiveDrainTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultDrainTimeout
	}
	return d
}

// waitOrTimeout blocks until done is closed or timeout elapses. Returns
// nil on close, errDrainTimeout on timeout. A zero or negative timeout
// falls back to defaultDrainTimeout. Used to bound the NATS shutdown
// wait and the Fossil drain so a stuck server cannot hang the hub
// process forever (#158).
func waitOrTimeout(done <-chan struct{}, timeout time.Duration) error {
	select {
	case <-done:
		return nil
	case <-time.After(effectiveDrainTimeout(timeout)):
		return errDrainTimeout
	}
}

// hubLogTail reads the last N lines (capped to ~2KB) of hub.log and
// returns them prefixed with a newline + section header, ready for
// concatenation into an error message. Returns "" when hub.log is
// missing or empty so callers don't get an empty trailing section.
// Used to surface in-child seed/start failures to the parent's
// stderr, which is what SessionStart hooks actually capture.
func hubLogTail(p paths) string {
	const maxBytes = 2048
	logPath := filepath.Join(p.orchDir, "hub.log")
	data, err := os.ReadFile(logPath)
	if err != nil || len(data) == 0 {
		return ""
	}
	if len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
	}
	return "\n--- " + logPath + " (tail) ---\n" + string(data)
}

// spawnDetachedChild re-execs the current binary as a foreground hub
// child, redirects its stdout/stderr to .bones/hub.log, and
// returns once the child has bound both ports. On readiness failure,
// the child is killed and pid files are cleared.
func spawnDetachedChild(p paths, o opts) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("hub: locate own binary: %w", err)
	}

	// Open the combined log first so the child's early panics surface.
	hubLog, err := os.OpenFile(filepath.Join(p.orchDir, "hub.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("hub: open log: %w", err)
	}
	defer func() { _ = hubLog.Close() }()

	cmd := exec.Command(exe, "hub", "start",
		"--repo-port", strconv.Itoa(o.repoPort),
		"--coord-port", strconv.Itoa(o.coordPort),
		"--drain-timeout", effectiveDrainTimeout(o.drainTimeout).String(),
	)
	cmd.Dir = p.root
	cmd.Stdout = hubLog
	cmd.Stderr = hubLog
	cmd.Env = append(os.Environ(), detachEnv+"=1")
	// Detach from the parent's process group so the child survives the
	// parent's exit.
	cmd.SysProcAttr = sysProcAttrDetached()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("hub: spawn child: %w", err)
	}

	// The child writes pid files itself. Probe both ports until the
	// child reports ready. On failure, surface the tail of hub.log
	// alongside the timeout — without this the SessionStart hook
	// attaches "fossil child not ready: timeout" with no hint at the
	// real cause (#127), which is typically a seed failure logged in
	// hub.log only.
	fossilAddr := fmt.Sprintf("127.0.0.1:%d", o.repoPort)
	natsAddr := fmt.Sprintf("127.0.0.1:%d", o.coordPort)
	parentLog := openHubLog(p)
	defer parentLog.Close()
	if err := waitForTCP(fossilAddr, readyTimeout); err != nil {
		_ = cmd.Process.Kill()
		parentLog.Errorf("hub: child exited before repo port ready: %v", err)
		return fmt.Errorf("hub: fossil child not ready: %w%s",
			err, hubLogTail(p))
	}
	if err := waitForTCP(natsAddr, readyTimeout); err != nil {
		_ = cmd.Process.Kill()
		parentLog.Errorf("hub: child exited before coord port ready: %v", err)
		return fmt.Errorf("hub: nats child not ready: %w%s",
			err, hubLogTail(p))
	}

	// False-positive readiness defense (#138 item 1): the TCP probes
	// above pass when SOMETHING responds on the configured ports, not
	// when our child is the responder. If another process already
	// owned repoPort/coordPort, our child's bind failed, our child
	// exited, but the probes succeeded against the unrelated process.
	// Without this check, joinLogic would proceed thinking the hub is
	// up, then every verb downstream fails mysteriously against the
	// foreign service.
	//
	// Verify the recorded hub pid matches our child. The child writes
	// p.hubPid before NewHub, so by the time the fossil port responds,
	// the file should exist with the child's pid. Mismatch (or missing)
	// means port collision or a crashed child that never wrote the file.
	if recorded, ok := readPid(p.hubPid); !ok || recorded != cmd.Process.Pid {
		_ = cmd.Process.Kill()
		parentLog.Errorf("hub: repo port collision (recorded pid=%d, child pid=%d)",
			recorded, cmd.Process.Pid)
		return fmt.Errorf("hub: fossil port %s responded but %s does not "+
			"name our child (pid %d, recorded %d) — likely port "+
			"collision; another service is bound to the port%s",
			fossilAddr, p.hubPid, cmd.Process.Pid, recorded,
			hubLogTail(p))
	}

	// Capture pid before Release: on Unix, os.Process.release sets
	// p.Pid = -1 (Go stdlib src/os/exec_unix.go), so reading
	// cmd.Process.Pid after Release yields -1 instead of the child's
	// real pid (#148).
	pid := cmd.Process.Pid

	// Release the child so it isn't reaped when we return.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("hub: release child: %w", err)
	}

	fmt.Printf("hub: fossil at http://127.0.0.1:%d, nats at nats://127.0.0.1:%d (pid=%d)\n",
		o.repoPort, o.coordPort, pid)
	return nil
}

// StopOption configures Stop. Passed variadically so existing callers
// using Stop(root) continue to compile without change.
type StopOption func(*stopOpts)

type stopOpts struct {
	force bool
}

// WithForce overrides Stop's safety check that refuses a teardown
// while any swarm slot has a live leaf process (#157). With
// force=true, Stop proceeds regardless and the operator owns any
// active leaves that lose their cached NATS URL when the hub
// restarts on a different port. bones down passes WithForce(true)
// since it is an explicit destructive teardown that has already
// confirmed with the operator.
func WithForce(force bool) StopOption {
	return func(o *stopOpts) { o.force = force }
}

// ErrActiveSlots is returned by Stop when one or more swarm slots
// have live leaf processes that would be silently disconnected by
// tearing the hub down (#157). Use WithForce to override.
var ErrActiveSlots = errors.New("hub: active swarm slots present")

// Stop terminates the process recorded in hub.pid (written by Start)
// and removes the pid file. SIGTERM first; if the process is still
// alive after stopGrace, SIGKILL. The pid file is only removed once
// the process is confirmed dead so a follow-up Start cannot mistake
// an orphan-still-alive for "nothing to clean up" (#138).
//
// A missing or stale hub.pid is not an error: Stop is idempotent so
// callers can shut down without first checking whether Start ran.
//
// As a safety, Stop will not signal the calling process. If the
// recorded pid matches os.Getpid(), Stop only removes the pid file.
// The foreground Start has its own ctx-cancellation path; signaling
// self would terminate the caller before it could clean up.
//
// Active-slot guard (#157): without WithForce, Stop refuses if any
// .bones/swarm/<slot>/leaf.pid points at a live process. That guard
// surfaces ErrActiveSlots wrapped with the slot names so the operator
// can close them first or pass --force.
//
// URL files (.bones/hub-{fossil,nats}-url) are preserved across Stop
// so the next Start re-reads the previously-bound port via
// resolvePorts. When the port is still free, the new hub binds the
// same port and active leaves' cached NATS URLs keep working. Full
// teardown (bones down) clears the URL files separately by removing
// the entire .bones directory.
func Stop(root string, options ...StopOption) (err error) {
	so := stopOpts{}
	for _, fn := range options {
		fn(&so)
	}

	p, err := newPaths(root)
	if err != nil {
		return err
	}

	_, end := telemetry.RecordCommand(context.Background(), "hub.stop")
	defer func() { end(err) }()

	if !so.force {
		if names, listErr := activeSlotNames(root); listErr != nil {
			// Best-effort: a missing or unreadable swarm dir must not
			// block stop. Log and proceed as if no slots were active.
			fmt.Fprintf(os.Stderr,
				"hub: warning: could not enumerate active slots: %v\n", listErr)
		} else if len(names) > 0 {
			return fmt.Errorf(
				"%w: %s — close them first or pass --force",
				ErrActiveSlots, strings.Join(names, ", "))
		}
	}

	self := os.Getpid()
	if pid, ok := readPid(p.hubPid); ok && pid != self {
		terminateProcess(pid)
	}
	_ = os.Remove(p.hubPid)
	return nil
}

// activeSlotNames returns the names of swarm slots whose leaf.pid
// points at a still-alive process. Wraps slotgc.LiveSlots so Stop's
// active-slot check shares a single source of truth with bones down's
// orphan reaping (#157).
func activeSlotNames(root string) ([]string, error) {
	live, err := slotgc.LiveSlots(root)
	if err != nil {
		return nil, fmt.Errorf("list active slots: %w", err)
	}
	names := make([]string, 0, len(live))
	for _, s := range live {
		names = append(names, s.Name)
	}
	return names, nil
}

// acquireStartLock takes the workspace-scoped hub lock (#208) when
// the caller is not the detached child. The detached child must NOT
// contend for the lock — the parent still holds it for the lifetime
// of waitForTCP and only releases on return. Returns a release func
// (no-op when the caller is the detached child) plus any acquisition
// error, wrapped with the "hub: " prefix the rest of Start uses.
func acquireStartLock(root string, isDetachChild bool) (func(), error) {
	if isDetachChild {
		return func() {}, nil
	}
	rel, err := acquireWorkspaceLockFunc(root)
	if err != nil {
		return nil, fmt.Errorf("hub: %w", err)
	}
	return rel, nil
}

// terminateProcess sends SIGTERM, polls until stopGrace elapses, and
// escalates to SIGKILL if the process is still alive. Returns once
// the process is dead or the post-KILL wait expires; callers treat
// the result as best-effort. Mirrors registry.Reap so behavior is
// consistent across the two kill paths.
func terminateProcess(pid int) {
	if !pidIntIsLive(pid) {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		if !pidIntIsLive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
	for range 20 {
		if !pidIntIsLive(pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// paths holds every filesystem location the hub touches. Pulled into a
// single struct so tests and callers reason about layout in one place
// (Ousterhout: information hiding).
type paths struct {
	root        string
	orchDir     string
	logDir      string
	hubRepo     string
	hubPid      string
	fossilURL   string
	natsURL     string
	fossilLog   string
	natsLog     string
	coordStore  string
	fslckout    string
	fslSettings string
}

// HubFossilPath returns the on-disk path of the hub fossil for the
// given workspace root. Use this rather than building the path
// literally in cli/ so verbs survive future layout changes.
func HubFossilPath(root string) string {
	return filepath.Join(root, markerDirName, "hub.fossil")
}

// IsRunning reports whether a hub for the workspace at root is
// currently running. Returns (pid, true) when hub.pid exists and
// names a live process; (0, false) otherwise.
//
// Read-only. Used by cli/up to print accurate post-scaffold status
// without spawning anything (per ADR 0041 the hub is started lazily
// on first verb, not by `bones up`).
func IsRunning(root string) (int, bool) {
	p, err := newPaths(root)
	if err != nil {
		return 0, false
	}
	pid, ok := readPid(p.hubPid)
	if !ok || !pidIntIsLive(pid) {
		return 0, false
	}
	return pid, true
}

// newPaths derives the hub layout from the workspace root. The root must
// exist; the .bones subdirs are created lazily by Start.
func newPaths(root string) (paths, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return paths{}, fmt.Errorf("hub: resolve root: %w", err)
	}
	if info, err := os.Stat(abs); err != nil {
		return paths{}, fmt.Errorf("hub: root: %w", err)
	} else if !info.IsDir() {
		return paths{}, fmt.Errorf("hub: root %q is not a directory", abs)
	}
	orch := filepath.Join(abs, markerDirName)
	return paths{
		root:        abs,
		orchDir:     orch,
		logDir:      orch,
		hubRepo:     filepath.Join(orch, "hub.fossil"),
		hubPid:      filepath.Join(orch, "hub.pid"),
		fossilURL:   filepath.Join(orch, "hub-fossil-url"),
		natsURL:     filepath.Join(orch, "hub-nats-url"),
		fossilLog:   filepath.Join(orch, "fossil.log"),
		natsLog:     filepath.Join(orch, "nats.log"),
		coordStore:  filepath.Join(orch, "coord"),
		fslckout:    filepath.Join(abs, ".fslckout"),
		fslSettings: filepath.Join(abs, ".fossil-settings"),
	}, nil
}

// pidIsLive returns true if pidFile holds a pid whose process is alive.
// Returns false on any error (missing file, malformed contents, dead pid).
func pidIsLive(pidFile string) bool {
	pid, ok := readPid(pidFile)
	if !ok {
		return false
	}
	return pidIntIsLive(pid)
}

// pidIntIsLive reports whether pid names a live process. Same probe
// pidIsLive uses, but takes the pid directly so callers that already
// have an integer (e.g., the Stop escalator) don't round-trip through
// a pid file.
func pidIntIsLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 is the standard "is this pid still around" probe. The
	// kernel performs the lookup but delivers nothing.
	return proc.Signal(syscall.Signal(0)) == nil
}

// readPid parses pidFile as a single integer. Whitespace is trimmed.
// Returns (0, false) on any read or parse error.
func readPid(pidFile string) (int, bool) {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	return pid, true
}

// writePid serializes pid into pidFile with 0644 perms.
func writePid(pidFile string, pid int) error {
	return os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// seedHubRepo creates a fresh Fossil repository at p.hubRepo, opens a
// checkout at p.root, adds every git-tracked file, and commits with a
// session-base message. Mirrors lines 32-46 of the legacy bash script.
//
// Returns an error if the working tree has no git-tracked files, since
// committing nothing would produce a checkout with an empty tip and the
// orchestrator workflow assumes a non-empty session base.

// seedHubRepoFunc is a package-level seam over seedHubRepo so tests can
// inject failure modes (e.g., to verify rollback of partially-created
// SQLite sidecars). Production code reaches the real implementation.
var seedHubRepoFunc = seedHubRepo

// waitForTCP polls addr until something is accepting connections or
// timeout elapses. Used for detach-mode parent-side readiness probes
// against the child's fossil HTTP and NATS listeners.
func waitForTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, pollInterval)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("timeout waiting for %s after %s", addr, timeout)
}

// removeRepoAndSidecars deletes the hub.fossil file and its SQLite
// -shm / -wal sidecars together. The three travel as a unit: deleting
// only hub.fossil leaves the sidecars to poison the next CreateWithEnv
// with SQLITE_IOERR_SHORT_READ (522). See #138.
func removeRepoAndSidecars(p paths) {
	for _, path := range []string{
		p.hubRepo,
		p.hubRepo + "-shm",
		p.hubRepo + "-wal",
		p.fslckout,
		p.fslSettings,
	} {
		_ = os.RemoveAll(path)
	}
}

func seedHubRepo(ctx context.Context, h *edgehub.Hub, p paths) error {
	files, err := gitTrackedFiles(p.root)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no git-tracked files to seed from")
	}

	shortSHA, err := gitShortSHA(p.root)
	if err != nil {
		return err
	}

	// Build FileToCommit list from every tracked path. Reading every
	// file into memory mirrors the bash flow (xargs fossil add); for
	// the workspace sizes we target (single repo, hundreds of MB at
	// most) this is fine.
	commitFiles := make([]edgehub.FileToCommit, 0, len(files))
	for _, rel := range files {
		abs := filepath.Join(p.root, rel)
		info, err := os.Lstat(abs)
		if err != nil {
			return fmt.Errorf("stat %s: %w", rel, err)
		}
		// Skip symlinks and non-regular files; the bash script's
		// `fossil add` handled these as a no-op via fossil's own
		// filters.
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		perm := ""
		if info.Mode()&0o111 != 0 {
			perm = "x"
		}
		commitFiles = append(commitFiles, edgehub.FileToCommit{
			Name:    rel,
			Content: data,
			Perm:    perm,
		})
	}

	_, err = h.Commit(ctx, edgehub.CommitOpts{
		Files:   commitFiles,
		Message: fmt.Sprintf("session base: %s", shortSHA),
		Author:  "orchestrator",
		Time:    time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ErrSeedPrecondition is returned by Start when the workspace has no
// git-tracked files for seedHubRepo to commit. Surfaced before the
// parent spawns the detached child so the user sees the real error
// (and an actionable next step) without waiting out the TCP-probe
// readyTimeout. See #138 item 9.
var ErrSeedPrecondition = errors.New(
	"no git-tracked files to seed hub fossil from; commit at least one " +
		"file (`git add . && git commit -m init`) before running " +
		"bones hub start")

// checkSeedPrecondition validates that seedHubRepo will succeed before
// the hub is spawned. Today the only precondition is that the
// workspace has at least one git-tracked file; future preconditions
// (e.g., libfossil version compatibility) belong here too.
func checkSeedPrecondition(root string) error {
	files, err := gitTrackedFiles(root)
	if err != nil {
		// Not a git repo, or git unavailable. Pass through with a
		// hub: prefix; the underlying error message already names
		// `git ls-files` which is enough for the user to act on.
		return fmt.Errorf("hub: seed precondition: %w", err)
	}
	if len(files) == 0 {
		return ErrSeedPrecondition
	}
	return nil
}

// gitTrackedFiles returns the workspace's git-tracked file paths
// (relative to root) in deterministic order. Equivalent to
// `git ls-files` from root.
func gitTrackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// gitShortSHA returns the abbreviated commit hash of HEAD, matching the
// `git rev-parse --short HEAD` invocation in the bash script.
func gitShortSHA(root string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// startFossil opens the seeded repo and runs ServeHTTP in a goroutine.
// Returns a cancel func that stops the server, a done channel that
// closes when the server goroutine exits, and the readiness check has
// already succeeded by the time this returns.
