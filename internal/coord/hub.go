// coord/hub.go
package coord

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	edgehub "github.com/danmestas/EdgeSync/hub"

	"github.com/danmestas/bones/internal/assert"
)

// Hub owns the orchestrator's hub fossil repo + HTTP xfer endpoint and
// the embedded NATS server. Wraps EdgeSync's hub package end-to-end:
// bones holds no libfossil or nats-server types, no per-coord
// repo/server lifecycle. NobodyCaps="gio" grants unauthenticated leaf
// clone/pull/push at hub bootstrap; DisableFossilSyncOverNATS is left
// at default false so the hub's NATS subscriber on
// "fossil.<project-code>.sync" continues to accept pushes from peer
// leaves over the leaf-mesh (the swarm flow's commit-propagation path).
type Hub struct {
	hub      *edgehub.Hub
	httpAddr string
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	stopped  bool
}

// OpenHub starts a hub at workdir/hub.fossil that serves HTTP on
// httpAddr (e.g. "127.0.0.1:8765") and runs an embedded NATS server
// with the fossil-sync subscriber enabled. The hub is a passive
// receiver of pushes from peer leaves; it never client-syncs out.
//
// httpAddr's port is honored when free; the hub package falls back to
// auto-pick if bind fails. The actual address is exposed via HTTPAddr().
//
// workdir is created if missing; hub.fossil is created if missing
// with NobodyCaps="gio" so unauthenticated leaf clones work without
// per-leaf credential plumbing.
func OpenHub(ctx context.Context, workdir, httpAddr string) (*Hub, error) {
	assert.NotNil(ctx, "coord.OpenHub: ctx is nil")
	assert.NotEmpty(workdir, "coord.OpenHub: workdir is empty")
	assert.NotEmpty(httpAddr, "coord.OpenHub: httpAddr is empty")

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, fmt.Errorf("coord.OpenHub: mkdir workdir: %w", err)
	}
	port, err := portFromAddr(httpAddr)
	if err != nil {
		return nil, fmt.Errorf("coord.OpenHub: %w", err)
	}
	h, err := edgehub.NewHub(ctx, edgehub.Config{
		RepoPath:       filepath.Join(workdir, "hub.fossil"),
		BootstrapUser:  "hub",
		NobodyCaps:     "gio",
		NATSStoreDir:   filepath.Join(workdir, "coord"),
		FossilHTTPPort: port,
	})
	if err != nil {
		return nil, fmt.Errorf("coord.OpenHub: %w", err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = h.ServeHTTP(serveCtx)
		close(done)
	}()
	return &Hub{
		hub:      h,
		httpAddr: h.HTTPAddr(),
		cancel:   cancel,
		done:     done,
	}, nil
}

// portFromAddr extracts the port from a host:port string. Returns 0 for
// auto-pick when the input is empty or has no port.
func portFromAddr(addr string) (int, error) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return 0, nil
	}
	var port int
	if _, err := fmt.Sscanf(addr[idx+1:], "%d", &port); err != nil {
		return 0, fmt.Errorf("parse port from %q: %w", addr, err)
	}
	return port, nil
}

// NATSURL returns the hub's NATS client URL.
func (h *Hub) NATSURL() string {
	assert.NotNil(h, "coord.Hub.NATSURL: receiver is nil")
	return h.hub.NATSURL()
}

// LeafUpstream returns the URL remote agents pass as NATSUpstream to
// peer their meshes into the hub's mesh as leaf nodes.
func (h *Hub) LeafUpstream() string {
	assert.NotNil(h, "coord.Hub.LeafUpstream: receiver is nil")
	return h.hub.LeafUpstream()
}

// HTTPAddr returns the hub's HTTP listen address.
func (h *Hub) HTTPAddr() string {
	assert.NotNil(h, "coord.Hub.HTTPAddr: receiver is nil")
	return "http://" + h.httpAddr
}

// Stop shuts down the embedded NATS server and HTTP listener. Safe to
// call more than once; subsequent calls are no-ops.
func (h *Hub) Stop() error {
	assert.NotNil(h, "coord.Hub.Stop: receiver is nil")
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return nil
	}
	h.stopped = true
	if h.cancel != nil {
		h.cancel()
	}
	if h.done != nil {
		<-h.done
	}
	if h.hub != nil {
		if err := h.hub.Stop(); err != nil {
			return fmt.Errorf("coord.Hub.Stop: %w", err)
		}
	}
	return nil
}
