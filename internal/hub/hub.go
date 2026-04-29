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
)

// detachEnv, when set, signals to a fork-exec'd child process that it
// should run Start in foreground regardless of the --detach flag. The
// CLI sets this on the child to break the recursion.
const detachEnv = "BONES_HUB_FOREGROUND"

// readyTimeout bounds how long Start waits for each server to accept
// connections. Mirrors EdgeSync's leaf-agent budget.
const readyTimeout = 5 * time.Second

// pollInterval is how often readiness probes retry. Short enough to keep
// total wakeup latency low, long enough not to busy-spin on the listener.
const pollInterval = 25 * time.Millisecond

// Start brings up the orchestrator hub: a Fossil repository at
// .orchestrator/hub.fossil seeded from git-tracked files, a Fossil HTTP
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
func Start(ctx context.Context, root string, options ...Option) error {
	o := defaults()
	for _, fn := range options {
		fn(&o)
	}
	// Re-entry from the fork-exec'd child: even if the user passed
	// WithDetach(true), the BONES_HUB_FOREGROUND env var (set by our
	// fork) forces foreground so we don't recurse.
	if os.Getenv(detachEnv) == "1" {
		o.detach = false
	}

	p, err := newPaths(root)
	if err != nil {
		return err
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
	if !pidIsLive(p.fossilPid) {
		for _, path := range []string{p.hubRepo, p.fslckout, p.fslSettings} {
			_ = os.RemoveAll(path)
		}
	}

	if _, err := os.Stat(p.hubRepo); errors.Is(err, os.ErrNotExist) {
		if err := seedHubRepo(p); err != nil {
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
	<-ctx.Done()
	fossilCancel()
	natsSrv.Shutdown()
	natsSrv.WaitForShutdown()
	<-fossilDone
	_ = os.Remove(p.fossilPid)
	_ = os.Remove(p.natsPid)
	return nil
}

// spawnDetachedChild re-execs the current binary as a foreground hub
// child, redirects its stdout/stderr to .orchestrator/hub.log, and
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
	// child reports ready.
	fossilAddr := fmt.Sprintf("127.0.0.1:%d", o.fossilPort)
	natsAddr := fmt.Sprintf("127.0.0.1:%d", o.natsPort)
	if err := waitForTCP(fossilAddr, readyTimeout); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("hub: fossil child not ready: %w", err)
	}
	if err := waitForTCP(natsAddr, readyTimeout); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("hub: nats child not ready: %w", err)
	}

	// Release the child so it isn't reaped when we return.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("hub: release child: %w", err)
	}

	fmt.Printf("hub: fossil at http://127.0.0.1:%d, nats at nats://127.0.0.1:%d (pid=%d)\n",
		o.fossilPort, o.natsPort, cmd.Process.Pid)
	return nil
}

// Stop sends SIGTERM to the processes recorded in the pid files written
// by Start and removes those pid files. Missing pid files or stale pids
// are not an error: Stop is idempotent so callers can shut down without
// first checking whether Start has run.
//
// As a safety, Stop will not signal the calling process. If the recorded
// pid is the same as os.Getpid(), Stop only removes the pid file. The
// foreground Start has its own ctx-cancellation path; signaling self
// would terminate the caller before it could clean up.
func Stop(root string) error {
	p, err := newPaths(root)
	if err != nil {
		return err
	}

	self := os.Getpid()
	// Both servers share a single child process in the detached model,
	// so the two pid files typically reference the same pid. Dedup so
	// we only signal once.
	signaled := make(map[int]struct{})
	for _, pidFile := range []string{p.fossilPid, p.natsPid} {
		pid, ok := readPid(pidFile)
		if ok && pid != self {
			if _, done := signaled[pid]; !done {
				if proc, err := os.FindProcess(pid); err == nil {
					_ = proc.Signal(syscall.SIGTERM)
				}
				signaled[pid] = struct{}{}
			}
		}
		_ = os.Remove(pidFile)
	}
	return nil
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
	fossilLog   string
	natsLog     string
	natsStore   string
	fslckout    string
	fslSettings string
}

// newPaths derives the hub layout from the workspace root. The root must
// exist; the orchestrator subdirs are created lazily by Start.
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
	orch := filepath.Join(abs, ".orchestrator")
	return paths{
		root:        abs,
		orchDir:     orch,
		pidDir:      filepath.Join(orch, "pids"),
		logDir:      orch,
		hubRepo:     filepath.Join(orch, "hub.fossil"),
		fossilPid:   filepath.Join(orch, "pids", "fossil.pid"),
		natsPid:     filepath.Join(orch, "pids", "nats.pid"),
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
