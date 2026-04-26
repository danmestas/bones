// coord/hub.go
package coord

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/danmestas/EdgeSync/leaf/agent"
	"github.com/danmestas/libfossil"

	"github.com/danmestas/agent-infra/internal/assert"
)

// Hub owns the orchestrator's hub fossil repo + HTTP xfer endpoint and
// runs an embedded NATS JetStream server. Leaves point at HTTPAddr() for
// fossil clone/sync and at NATSURL() for leaf-to-leaf NATS sync.
//
// Hub wraps a *leaf.Agent with serve flags set (Pull=false, Push=false,
// Autosync=AutosyncOff). The agent's serve_http handler exposes
// repo.XferHandler() so a stock fossil client can pull/push. The
// embedded NATS server is run by Hub itself (not the agent's mesh)
// because leaves need a NATS URL they can dial without solving
// EdgeSync's leaf-mode handshake from outside the process.
type Hub struct {
	agent    *agent.Agent
	natsSrv  *natsserver.Server
	storeDir string
	httpAddr string
	mu       sync.Mutex
	stopped  bool
}

// OpenHub starts a hub at workdir/hub.fossil that serves HTTP on
// httpAddr (e.g. "127.0.0.1:8765") and runs an embedded NATS JetStream
// server bound to a random localhost port. The hub is a passive
// receiver of pushes from peer leaves.
//
// workdir is created if missing; hub.fossil is created if missing.
// Caller owns workdir and is responsible for cleanup.
func OpenHub(ctx context.Context, workdir, httpAddr string) (*Hub, error) {
	assert.NotNil(ctx, "coord.OpenHub: ctx is nil")
	assert.NotEmpty(workdir, "coord.OpenHub: workdir is empty")
	assert.NotEmpty(httpAddr, "coord.OpenHub: httpAddr is empty")

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, fmt.Errorf("coord.OpenHub: mkdir workdir: %w", err)
	}
	repoPath := filepath.Join(workdir, "hub.fossil")
	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		r, cerr := libfossil.Create(repoPath, libfossil.CreateOpts{User: "hub"})
		if cerr != nil {
			return nil, fmt.Errorf("coord.OpenHub: create repo: %w", cerr)
		}
		// Grant the unauthenticated user clone/pull/push caps so leaf
		// agents can sync without coord plumbing per-leaf credentials.
		// libfossil's xfer handler treats requests with no login card
		// as user "nobody" (see internal/sync/handler.go initAuth).
		// libfossil.Create pre-populates "nobody" with empty caps;
		// SetCaps grants 'gio' (clone, pull, push).
		if cerr := r.SetCaps("nobody", "gio"); cerr != nil {
			_ = r.Close()
			return nil, fmt.Errorf("coord.OpenHub: grant nobody caps: %w", cerr)
		}
		_ = r.Close()
	}

	natsStoreDir := filepath.Join(workdir, "nats-store")
	srv, err := startEmbeddedNATS(natsStoreDir)
	if err != nil {
		return nil, fmt.Errorf("coord.OpenHub: nats: %w", err)
	}

	cfg := agent.Config{
		RepoPath:         repoPath,
		NATSUpstream:     srv.ClientURL(),
		ServeHTTPAddr:    httpAddr,
		ServeNATSEnabled: true,
		Pull:             false,
		Push:             false,
		Autosync:         agent.AutosyncOff,
	}
	a, err := agent.New(cfg)
	if err != nil {
		srv.Shutdown()
		return nil, fmt.Errorf("coord.OpenHub: agent.New: %w", err)
	}
	if err := a.Start(); err != nil {
		_ = a.Stop()
		srv.Shutdown()
		return nil, fmt.Errorf("coord.OpenHub: agent.Start: %w", err)
	}
	return &Hub{
		agent:    a,
		natsSrv:  srv,
		storeDir: natsStoreDir,
		httpAddr: httpAddr,
	}, nil
}

// startEmbeddedNATS launches a localhost-only JetStream NATS server with
// state under storeDir. The store dir must persist across hub restarts
// in production; tests pass a t.TempDir() and let cleanup handle it.
func startEmbeddedNATS(storeDir string) (*natsserver.Server, error) {
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir store: %w", err)
	}
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  storeDir,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("new server: %w", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		return nil, fmt.Errorf("nats not ready")
	}
	return srv, nil
}

// NATSURL returns the embedded NATS server's client URL. Leaves use this
// as `OpenLeaf`'s hubNATSURL argument.
func (h *Hub) NATSURL() string {
	assert.NotNil(h, "coord.Hub.NATSURL: receiver is nil")
	return h.natsSrv.ClientURL()
}

// HTTPAddr returns the hub's HTTP listen address, suitable as the
// hubHTTPAddr argument to OpenLeaf.
func (h *Hub) HTTPAddr() string {
	assert.NotNil(h, "coord.Hub.HTTPAddr: receiver is nil")
	return "http://" + h.httpAddr
}

// Stop shuts down the agent and the embedded NATS server. Safe to call
// more than once; subsequent calls are no-ops.
func (h *Hub) Stop() error {
	assert.NotNil(h, "coord.Hub.Stop: receiver is nil")
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return nil
	}
	h.stopped = true
	var firstErr error
	if h.agent != nil {
		if err := h.agent.Stop(); err != nil {
			firstErr = fmt.Errorf("coord.Hub.Stop: agent: %w", err)
		}
	}
	if h.natsSrv != nil {
		h.natsSrv.Shutdown()
		h.natsSrv.WaitForShutdown()
	}
	if h.storeDir != "" {
		_ = os.RemoveAll(h.storeDir)
	}
	return firstErr
}
