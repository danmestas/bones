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

	"github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/modernc"
	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/slotgc"
	"github.com/danmestas/bones/internal/telemetry"
)

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

// Start brings up the orchestrator hub: a Fossil repository at
// .bones/hub.fossil seeded from git-tracked files, a Fossil HTTP
// server on the chosen port, and an embedded NATS JetStream server.
//
// Idempotent: if both pid files exist and the recorded processes are
// alive, Start returns nil immediately.
//
// With WithDetach(true) the calling process fork-execs itself in
// "foreground" mode, waits for both servers to become reachable, and
// returns. The child outlives the caller and owns the servers; pid
// files reference the child. This is what `bones hub start --detach`
// uses so a shell can fire-and-forget the hub.
//
// Without detach, Start blocks on ctx.Done(): the calling process is
// the hub. Pid files reference the calling process. On cancellation,
// both servers shut down cleanly and pid files are removed.
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

	p, err := newPaths(root)
	if err != nil {
		return err
	}

	// Telemetry: record only the parent's Start. The detached child
	// re-enters Start with BONES_HUB_FOREGROUND=1 and would otherwise
	// emit a daemon-lifetime span the parent already covered.
	if !isDetachChild {
		urlRecorded := readURLFile(p.fossilURL) != ""
		var end telemetry.EndFunc
		ctx, end = telemetry.RecordCommand(ctx, "hub.start",
			telemetry.Bool("detach", o.detach),
			telemetry.Bool("url_recorded", urlRecorded),
		)
		defer func() { end(err) }()
	}

	if err := os.MkdirAll(p.pidDir, 0o755); err != nil {
		return fmt.Errorf("hub: pids dir: %w", err)
	}
	if err := os.MkdirAll(p.logDir, 0o755); err != nil {
		return fmt.Errorf("hub: logs dir: %w", err)
	}

	// Idempotency: if both pid files point at live processes, we're done.
	if pidIsLive(p.fossilPid) && pidIsLive(p.natsPid) {
		return nil
	}

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
	// Fresh-start detection (ADR 0023): if no fossil PID is alive,
	// wipe stale checkout state so each session starts from a clean
	// substrate. Working-tree files are untouched.
	//
	// SQLite -shm and -wal sidecars are part of "stale checkout state":
	// when a prior run crashed mid-seed, libfossil leaves a 0-byte WAL
	// and a populated SHM. The next CreateWithEnv hits
	// SQLITE_IOERR_SHORT_READ (522) on those orphan blocks. See #138.
	if !pidIsLive(p.fossilPid) {
		removeRepoAndSidecars(p)
	}

	if _, err := os.Stat(p.hubRepo); errors.Is(err, os.ErrNotExist) {
		if err := seedHubRepoFunc(p); err != nil {
			// Roll back any partial repo state — libfossil may have
			// created hub.fossil and/or its -shm/-wal sidecars before
			// the schema apply step that failed. Leaving those in
			// place poisons the next bones hub start (#138).
			removeRepoAndSidecars(p)
			return fmt.Errorf("hub: seed: %w", err)
		}
	}

	fossilCancel, fossilDone, err := startFossil(p, o.fossilPort)
	if err != nil {
		return fmt.Errorf("hub: fossil: %w", err)
	}

	natsSrv, err := startNATS(p, o.natsPort)
	if err != nil {
		fossilCancel()
		<-fossilDone
		_ = os.Remove(p.fossilPid)
		return fmt.Errorf("hub: nats: %w", err)
	}

	fmt.Printf("hub: fossil at http://127.0.0.1:%d, nats at nats://127.0.0.1:%d\n",
		o.fossilPort, o.natsPort)

	// Record this workspace in the cross-workspace registry so
	// `bones status --all` can discover running hubs. Non-fatal: the hub
	// still works without registry visibility.
	if err := registry.Write(registry.Entry{
		Cwd:       p.root,
		Name:      filepath.Base(p.root),
		HubURL:    fmt.Sprintf("http://127.0.0.1:%d", o.fossilPort),
		NATSURL:   fmt.Sprintf("nats://127.0.0.1:%d", o.natsPort),
		HubPID:    os.Getpid(),
		StartedAt: time.Now().UTC(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "hub: registry write failed (non-fatal): %v\n", err)
	}

	<-ctx.Done()
	fossilCancel()
	natsSrv.Shutdown()
	natsSrv.WaitForShutdown()
	<-fossilDone
	_ = os.Remove(p.fossilPid)
	_ = os.Remove(p.natsPid)
	return nil
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
		"--fossil-port", strconv.Itoa(o.fossilPort),
		"--nats-port", strconv.Itoa(o.natsPort),
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
	fossilAddr := fmt.Sprintf("127.0.0.1:%d", o.fossilPort)
	natsAddr := fmt.Sprintf("127.0.0.1:%d", o.natsPort)
	if err := waitForTCP(fossilAddr, readyTimeout); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("hub: fossil child not ready: %w%s",
			err, hubLogTail(p))
	}
	if err := waitForTCP(natsAddr, readyTimeout); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("hub: nats child not ready: %w%s",
			err, hubLogTail(p))
	}

	// Release the child so it isn't reaped when we return.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("hub: release child: %w", err)
	}

	fmt.Printf("hub: fossil at http://127.0.0.1:%d, nats at nats://127.0.0.1:%d (pid=%d)\n",
		o.fossilPort, o.natsPort, cmd.Process.Pid)
	return nil
}

// Stop terminates the processes recorded in the pid files written by
// Start and removes those pid files. SIGTERM first; if the process is
// still alive after stopGrace, SIGKILL. Pid files are only removed
// once the process is confirmed dead so a follow-up Start cannot
// mistake an orphan-still-alive for "nothing to clean up" (#138).
//
// Missing pid files or stale pids are not an error: Stop is idempotent
// so callers can shut down without first checking whether Start ran.
//
// As a safety, Stop will not signal the calling process. If the
// recorded pid matches os.Getpid(), Stop only removes the pid file.
// The foreground Start has its own ctx-cancellation path; signaling
// self would terminate the caller before it could clean up.
func Stop(root string) (err error) {
	p, err := newPaths(root)
	if err != nil {
		return err
	}

	_, end := telemetry.RecordCommand(context.Background(), "hub.stop")
	defer func() { end(err) }()

	self := os.Getpid()
	// Both servers share a single child process in the detached model,
	// so the two pid files typically reference the same pid. Dedup so
	// we only signal once.
	handled := make(map[int]struct{})
	for _, pidFile := range []string{p.fossilPid, p.natsPid} {
		pid, ok := readPid(pidFile)
		if ok && pid != self {
			if _, done := handled[pid]; !done {
				terminateProcess(pid)
				handled[pid] = struct{}{}
			}
		}
		_ = os.Remove(pidFile)
	}
	// Clear the recorded URL files so subsequent FossilURL/NATSURL
	// callers see the hub as down.
	_ = os.Remove(p.fossilURL)
	_ = os.Remove(p.natsURL)
	return nil
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
	pidDir      string
	logDir      string
	hubRepo     string
	fossilPid   string
	natsPid     string
	fossilURL   string
	natsURL     string
	fossilLog   string
	natsLog     string
	natsStore   string
	fslckout    string
	fslSettings string
}

// HubFossilPath returns the on-disk path of the hub fossil for the
// given workspace root. Use this rather than building the path
// literally in cli/ so verbs survive future layout changes.
func HubFossilPath(root string) string {
	return filepath.Join(root, markerDirName, "hub.fossil")
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
		pidDir:      filepath.Join(orch, "pids"),
		logDir:      orch,
		hubRepo:     filepath.Join(orch, "hub.fossil"),
		fossilPid:   filepath.Join(orch, "pids", "fossil.pid"),
		natsPid:     filepath.Join(orch, "pids", "nats.pid"),
		fossilURL:   filepath.Join(orch, "hub-fossil-url"),
		natsURL:     filepath.Join(orch, "hub-nats-url"),
		fossilLog:   filepath.Join(orch, "fossil.log"),
		natsLog:     filepath.Join(orch, "nats.log"),
		natsStore:   filepath.Join(orch, "nats-store"),
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

func seedHubRepo(p paths) error {
	r, err := libfossil.Create(p.hubRepo, libfossil.CreateOpts{User: "orchestrator"})
	if err != nil {
		return fmt.Errorf("create repo: %w", err)
	}
	_ = r.Close()

	files, err := gitTrackedFiles(p.root)
	if err != nil {
		_ = os.Remove(p.hubRepo)
		return err
	}
	if len(files) == 0 {
		_ = os.Remove(p.hubRepo)
		return errors.New("no git-tracked files to seed from")
	}

	shortSHA, err := gitShortSHA(p.root)
	if err != nil {
		_ = os.Remove(p.hubRepo)
		return err
	}

	repo, err := libfossil.Open(p.hubRepo)
	if err != nil {
		return fmt.Errorf("reopen repo: %w", err)
	}
	defer func() { _ = repo.Close() }()

	// Build FileToCommit list from every tracked path. Reading every
	// file into memory mirrors the bash flow (xargs fossil add); for
	// the workspace sizes we target (single repo, hundreds of MB at
	// most) this is fine. If it ever isn't, switch to OpenCheckout +
	// Add + Checkin which streams off disk.
	commitFiles := make([]libfossil.FileToCommit, 0, len(files))
	for _, rel := range files {
		abs := filepath.Join(p.root, rel)
		info, err := os.Lstat(abs)
		if err != nil {
			return fmt.Errorf("stat %s: %w", rel, err)
		}
		// Skip symlinks and non-regular files: libfossil's commit path
		// expects bytes, and the bash script's `fossil add` handled
		// these as a no-op via fossil's own filters.
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
		commitFiles = append(commitFiles, libfossil.FileToCommit{
			Name:    rel,
			Content: data,
			Perm:    perm,
		})
	}

	_, _, err = repo.Commit(libfossil.CommitOpts{
		Files:   commitFiles,
		Comment: fmt.Sprintf("session base: %s", shortSHA),
		User:    "orchestrator",
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
func startFossil(p paths, port int) (cancel context.CancelFunc, done chan struct{}, err error) {
	repo, err := libfossil.Open(p.hubRepo)
	if err != nil {
		return nil, nil, fmt.Errorf("open repo: %w", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ctx, cancelFn := context.WithCancel(context.Background())
	done = make(chan struct{})

	// libfossil.ServeHTTP blocks until ctx is canceled. Run it in a
	// goroutine and probe the listener for readiness.
	go func() {
		defer close(done)
		defer func() { _ = repo.Close() }()
		_ = repo.ServeHTTP(ctx, addr)
	}()

	if err := waitForTCP(addr, readyTimeout); err != nil {
		cancelFn()
		<-done
		return nil, nil, fmt.Errorf("fossil readiness: %w", err)
	}

	if err := writePid(p.fossilPid, os.Getpid()); err != nil {
		cancelFn()
		<-done
		return nil, nil, fmt.Errorf("write fossil.pid: %w", err)
	}
	return cancelFn, done, nil
}

// startNATS launches an embedded NATS server with JetStream rooted at
// p.natsStore. Equivalent to `nats-server -js -p <port>`. The store
// directory persists across hub restarts so JetStream streams survive a
// reopen — the bash flow relied on the OS process owning that state.
//
// Retries readiness up to natsBootstrapAttempts times with exponential
// backoff between attempts (ADR 0034). The single-attempt 5s probe in
// the original implementation was the documented cause of operators
// believing bones was unreliable on loaded machines.
func startNATS(p paths, port int) (*natsserver.Server, error) {
	if err := os.MkdirAll(p.natsStore, 0o755); err != nil {
		return nil, fmt.Errorf("nats store dir: %w", err)
	}
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      port,
		JetStream: true,
		StoreDir:  p.natsStore,
		// Use a stable name so JetStream metadata is reusable across
		// restarts. NoLog/NoSigs differ from the test fixture: this is
		// a daemon, not a test server, so we want logs and we don't
		// want NATS to install signal handlers (the parent process owns
		// shutdown via Stop / ctx).
		NoSigs:     true,
		ServerName: "bones-hub",
	}

	backoff := natsBootstrapBackoff
	var lastErr error
	for attempt := 1; attempt <= natsBootstrapAttempts; attempt++ {
		srv, err := tryStartNATS(opts, p)
		if err == nil {
			return srv, nil
		}
		lastErr = err
		if attempt < natsBootstrapAttempts {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return nil, fmt.Errorf("nats bootstrap failed after %d attempts: %w",
		natsBootstrapAttempts, lastErr)
}

// tryStartNATS performs one bootstrap attempt. Caller retries on
// failure. A failed attempt fully tears down the server so the
// retry sees a clean state; this is correct because NATS holds
// listening sockets that would otherwise block the next attempt.
func tryStartNATS(opts *natsserver.Options, p paths) (*natsserver.Server, error) {
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("new server: %w", err)
	}

	go srv.Start()
	if !srv.ReadyForConnections(readyTimeout) {
		srv.Shutdown()
		srv.WaitForShutdown()
		return nil, fmt.Errorf("nats not ready within %s", readyTimeout)
	}
	if err := writePid(p.natsPid, os.Getpid()); err != nil {
		srv.Shutdown()
		srv.WaitForShutdown()
		return nil, fmt.Errorf("write nats.pid: %w", err)
	}
	return srv, nil
}

// waitForTCP polls addr until something is accepting connections or
// timeout elapses. Used as the fossil-server readiness probe; libfossil
// does not currently expose a ReadyForConnections-style hook so we
// dial-test instead.
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
