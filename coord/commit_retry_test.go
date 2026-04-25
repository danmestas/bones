package coord_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/testutil/natstest"
	"github.com/danmestas/libfossil"
)

// TestCommit_RetriesAfterFork is the integration-level expectation for
// Phase 2's pull+update+retry behavior: when WouldFork fires inside
// Commit and HubURL is reachable, coord pulls from the hub, updates the
// checkout, and retries the commit once. This skips until Phase 3
// publisher/subscriber lands and surfaces the inter-agent fork that
// drives the retry path end-to-end.
func TestCommit_RetriesAfterFork(t *testing.T) {
	t.Skip("integration: enabled when Phase 3 publisher/subscriber lands")
	ctx := context.Background()
	hub, hubURL := startTestHub(t)
	defer hub.Close()
	nc, _ := natstest.NewJetStreamServer(t)

	cfgA := testCoordConfig(t, nc.ConnectedUrl(), hubURL, "agent-A")
	cfgB := testCoordConfig(t, nc.ConnectedUrl(), hubURL, "agent-B")
	a, err := coord.Open(ctx, cfgA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	defer a.Close()
	b, err := coord.Open(ctx, cfgB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	defer b.Close()

	taskID, _ := a.OpenTask(ctx, "shared", []string{"a/x.txt", "b/y.txt"})
	relA, _ := a.Claim(ctx, taskID, 30*time.Second)
	defer func() { _ = relA() }()
	relB, _ := b.Claim(ctx, taskID, 30*time.Second)
	defer func() { _ = relB() }()

	if _, err := a.Commit(ctx, taskID, "first", []coord.File{
		{Path: "a/x.txt", Content: []byte("A")},
	}); err != nil {
		t.Fatalf("A.Commit: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	rev, err := b.Commit(ctx, taskID, "second", []coord.File{
		{Path: "b/y.txt", Content: []byte("B")},
	})
	if err != nil {
		t.Fatalf("B.Commit: expected success after retry, got %v", err)
	}
	if rev == "" {
		t.Fatal("B.Commit: empty rev on success")
	}
}

// TestCommit_DoubleForkSurfaces locks the unrecoverable-fork contract:
// when a retry cannot resolve the fork (here simulated by an empty
// HubURL, which makes the first fork unrecoverable), Commit returns a
// ConflictForkedError with empty Branch and empty Rev. No fork branch
// is ever created — this is the Phase 2 behavior change.
func TestCommit_DoubleForkSurfaces(t *testing.T) {
	t.Skip("integration: enabled when Phase 3 publisher/subscriber lands")
	ctx := context.Background()
	c := openCoordWithAlwaysForkSubstrate(t)
	defer c.Close()

	taskID, _ := c.OpenTask(ctx, "task-1", []string{"f.txt"})
	rel, _ := c.Claim(ctx, taskID, 30*time.Second)
	defer func() { _ = rel() }()

	_, err := c.Commit(ctx, taskID, "msg", []coord.File{
		{Path: "f.txt", Content: []byte("x")},
	})
	if !errors.Is(err, coord.ErrConflictForked) {
		t.Fatalf("expected ErrConflictForked on double-fork, got %v", err)
	}
	var cfe *coord.ConflictForkedError
	if errors.As(err, &cfe) {
		if cfe.Branch != "" {
			t.Fatalf("expected empty Branch (no fork branch), got %q", cfe.Branch)
		}
	}
}

// --- Helpers (will be exercised once t.Skip is removed) ---

// startTestHub spins up an httptest server backed by a libfossil repo
// that handles sync-protocol payloads. The returned URL is a valid
// fossil sync endpoint that leaf Manager.Pull can dial.
func startTestHub(t *testing.T) (*testHub, string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := libfossil.Create(filepath.Join(dir, "hub.fossil"), libfossil.CreateOpts{})
	if err != nil {
		t.Fatalf("libfossil.Create: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		resp, err := repo.HandleSync(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(resp)
	}))
	return &testHub{repo: repo, srv: srv}, srv.URL
}

// testHub bundles the repo+server pair so the test can shut both down
// in the right order (server first, then close the repo handle).
type testHub struct {
	repo *libfossil.Repo
	srv  *httptest.Server
}

func (h *testHub) Close() {
	h.srv.Close()
	_ = h.repo.Close()
}

// testCoordConfig produces a Config aimed at a per-test Fossil repo and
// checkout root, with the embedded NATS URL and (optionally) a hub URL
// threaded through. Limits are deliberately small so leaks fail fast.
func testCoordConfig(t *testing.T, natsURL, hubURL, agent string) coord.Config {
	t.Helper()
	dir := t.TempDir()
	return coord.Config{
		AgentID:            agent,
		NATSURL:            natsURL,
		HubURL:             hubURL,
		EnableTipBroadcast: true,
		FossilRepoPath:     filepath.Join(dir, "leaf.fossil"),
		CheckoutRoot:       filepath.Join(dir, "wt"),
		ChatFossilRepoPath: filepath.Join(dir, "chat.fossil"),
		HoldTTLDefault:     30 * time.Second,
		HoldTTLMax:         60 * time.Second,
		MaxHoldsPerClaim:   8,
		MaxSubscribers:     8,
		MaxTaskFiles:       8,
		MaxReadyReturn:     32,
		MaxTaskValueSize:   8192,
		TaskHistoryDepth:   8,
		OperationTimeout:   30 * time.Second,
		HeartbeatInterval:  5 * time.Second,
		NATSReconnectWait:  100 * time.Millisecond,
		NATSMaxReconnects:  10,
	}
}

// openCoordWithAlwaysForkSubstrate produces a Coord whose HubURL is
// empty, so a single WouldFork=true forces the unrecoverable path
// without any retry attempt — exactly the surface
// TestCommit_DoubleForkSurfaces wants to drive.
func openCoordWithAlwaysForkSubstrate(t *testing.T) *coord.Coord {
	t.Helper()
	nc, _ := natstest.NewJetStreamServer(t)
	hub, _ := startTestHub(t)
	t.Cleanup(hub.Close)
	cfg := testCoordConfig(t, nc.ConnectedUrl(), "", "always-fork-agent")
	cfg.HubURL = "" // empty HubURL forces the unrecoverable-fork path in commit.go
	c, err := coord.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}
