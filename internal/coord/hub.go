// coord/hub.go
package coord

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/danmestas/EdgeSync/leaf/agent"
	"github.com/danmestas/libfossil"

	"github.com/danmestas/bones/internal/assert"
)

// Hub owns the orchestrator's hub fossil repo + HTTP xfer endpoint and
// exposes the leaf.Agent's embedded NATS mesh as the hub's NATS bus.
//
// Hub wraps a *leaf.Agent with serve flags set (Pull=false, Push=false,
// Autosync=AutosyncOff, ServeNATSEnabled=true). The agent's serve_http
// handler exposes repo.XferHandler() so a stock fossil client can
// pull/push, AND its NATS subscriber on fossil.<project-code>.sync
// processes pushes from peer leaves over the leaf-mesh.
//
// Why not the EdgeSync hub package: bones's swarm flow relies on
// NATS-based fossil sync (slot leaf publishes, hub agent subscribes
// and merges). The hub package's NewHub starts a NATS server but does
// not register the fossil-sync subscriber. Until the hub package gains
// that surface, this site stays on the leaf agent serve-mode pattern.
type Hub struct {
	agent    *agent.Agent
	httpAddr string
	mu       sync.Mutex
	stopped  bool
}

// OpenHub starts a hub at workdir/hub.fossil that serves HTTP on
// httpAddr (e.g. "127.0.0.1:8765") and runs the embedded leaf.Agent
// mesh NATS as the hub's NATS bus. The hub is a passive receiver of
// pushes from peer leaves; it never client-syncs out.
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
		if cerr := r.SetCaps("nobody", "gio"); cerr != nil {
			_ = r.Close()
			return nil, fmt.Errorf("coord.OpenHub: grant nobody caps: %w", cerr)
		}
		_ = r.Close()
	}

	cfg := agent.Config{
		RepoPath:         repoPath,
		NATSUpstream:     "",
		NATSStoreDir:     filepath.Join(workdir, ".nats-store"),
		ServeHTTPAddr:    httpAddr,
		ServeNATSEnabled: true,
		Pull:             false,
		Push:             false,
		Autosync:         agent.AutosyncOff,
	}
	a, err := agent.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("coord.OpenHub: agent.New: %w", err)
	}
	if err := a.Start(); err != nil {
		_ = a.Stop()
		return nil, fmt.Errorf("coord.OpenHub: agent.Start: %w", err)
	}
	return &Hub{
		agent:    a,
		httpAddr: httpAddr,
	}, nil
}

// NATSURL returns the hub's NATS client URL.
func (h *Hub) NATSURL() string {
	assert.NotNil(h, "coord.Hub.NATSURL: receiver is nil")
	return h.agent.MeshClientURL()
}

// LeafUpstream returns the URL remote agents pass as NATSUpstream to
// peer their meshes into the hub's mesh as leaf nodes.
func (h *Hub) LeafUpstream() string {
	assert.NotNil(h, "coord.Hub.LeafUpstream: receiver is nil")
	addr := h.agent.MeshLeafAddr()
	if addr == "" {
		return ""
	}
	return "nats://" + addr
}

// HTTPAddr returns the hub's HTTP listen address.
func (h *Hub) HTTPAddr() string {
	assert.NotNil(h, "coord.Hub.HTTPAddr: receiver is nil")
	return "http://" + h.httpAddr
}

// Stop shuts down the agent. Safe to call more than once.
func (h *Hub) Stop() error {
	assert.NotNil(h, "coord.Hub.Stop: receiver is nil")
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return nil
	}
	h.stopped = true
	if h.agent != nil {
		if err := h.agent.Stop(); err != nil {
			return fmt.Errorf("coord.Hub.Stop: agent: %w", err)
		}
	}
	return nil
}
