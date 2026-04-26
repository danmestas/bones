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

	"github.com/danmestas/agent-infra/internal/assert"
)

// Hub owns the orchestrator's hub fossil repo + HTTP xfer endpoint and
// exposes the leaf.Agent's embedded NATS mesh as the hub's NATS bus.
//
// Hub wraps a *leaf.Agent with serve flags set (Pull=false, Push=false,
// Autosync=AutosyncOff). The agent's serve_http handler exposes
// repo.XferHandler() so a stock fossil client can pull/push. The
// agent's mesh runs a standalone NATS server (no upstream); peer
// leaves solicit it via NATSUpstream and publish/subscribe land
// directly on the hub's mesh — single-hop subject-interest
// propagation. See EdgeSync PR #77 (MeshClientURL/MeshLeafAddr).
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

	cfg := agent.Config{
		RepoPath:         repoPath,
		NATSUpstream:     "", // hub's mesh runs standalone — peer leaves solicit it
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

// NATSURL returns the hub's NATS client URL — the agent's mesh accepts
// regular client connections here. Use this for non-agent NATS clients
// (e.g. coord's claim/task KV traffic from the same process). For
// remote agents joining the hub's mesh upstream, see LeafUpstream.
func (h *Hub) NATSURL() string {
	assert.NotNil(h, "coord.Hub.NATSURL: receiver is nil")
	return h.agent.MeshClientURL()
}

// LeafUpstream returns the URL remote agents pass as NATSUpstream to
// peer their meshes into the hub's mesh as leaf nodes. The hub mesh
// accepts these solicits on its leaf-node port (separate from the
// client port returned by NATSURL).
func (h *Hub) LeafUpstream() string {
	assert.NotNil(h, "coord.Hub.LeafUpstream: receiver is nil")
	addr := h.agent.MeshLeafAddr()
	if addr == "" {
		return ""
	}
	return "nats://" + addr
}

// HTTPAddr returns the hub's HTTP listen address, suitable as the
// hubHTTPAddr argument to OpenLeaf.
func (h *Hub) HTTPAddr() string {
	assert.NotNil(h, "coord.Hub.HTTPAddr: receiver is nil")
	return "http://" + h.httpAddr
}

// Stop shuts down the agent (which also shuts down its embedded NATS
// mesh). Safe to call more than once; subsequent calls are no-ops.
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
