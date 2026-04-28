package coord

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// presenceCfgForAgent returns a validConfigWithURL tweaked for a
// second agent on the same substrate: new AgentID and a namespaced
// ChatFossilRepoPath so two Coords on one tempdir don't collide on
// the Fossil repo.
func presenceCfgForAgent(
	t *testing.T, url, agentID string,
) Config {
	t.Helper()
	cfg := validConfigWithURL(t, url)
	cfg.AgentID = agentID
	cfg.ChatFossilRepoPath = filepath.Join(
		t.TempDir(), agentID+"-chat.fossil",
	)
	return cfg
}

func TestWho_Self(t *testing.T) {
	c := mustOpen(t)
	got, err := c.Who(context.Background())
	if err != nil {
		t.Fatalf("Who: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Who returned %d entries, want 1", len(got))
	}
	if got[0].AgentID() != "test-agent" {
		t.Fatalf(
			"Who AgentID = %q, want test-agent", got[0].AgentID(),
		)
	}
	if got[0].Project() != "test" {
		t.Fatalf(
			"Who Project = %q, want test", got[0].Project(),
		)
	}
	if got[0].LastSeen().IsZero() {
		t.Fatalf("Who LastSeen is zero")
	}
	if got[0].StartedAt().IsZero() {
		t.Fatalf("Who StartedAt is zero")
	}
}

func TestWho_TwoAgents(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()

	cA, err := Open(
		context.Background(), presenceCfgForAgent(t, url, "test-a"),
	)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	t.Cleanup(func() { _ = cA.Close() })
	cB, err := Open(
		context.Background(), presenceCfgForAgent(t, url, "test-b"),
	)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	t.Cleanup(func() { _ = cB.Close() })

	entries, err := cA.Who(context.Background())
	if err != nil {
		t.Fatalf("Who: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Who returned %d, want 2", len(entries))
	}
	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.AgentID()] = true
	}
	if !ids["test-a"] || !ids["test-b"] {
		t.Fatalf("Who missing an agent: %+v", entries)
	}
}

func TestWatchPresence_UpThenDown(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()

	cA, err := Open(
		context.Background(), presenceCfgForAgent(t, url, "test-a"),
	)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	t.Cleanup(func() { _ = cA.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, closer, err := cA.WatchPresence(ctx)
	if err != nil {
		t.Fatalf("WatchPresence: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	cB, err := Open(
		context.Background(), presenceCfgForAgent(t, url, "test-b"),
	)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}

	gotUp := false
	deadline := time.After(2 * time.Second)
UP:
	for {
		select {
		case e := <-events:
			pc, ok := e.(PresenceChange)
			if !ok {
				t.Fatalf("WatchPresence: got non-PresenceChange %T", e)
			}
			if pc.AgentID() == "test-b" && pc.Up() {
				gotUp = true
				break UP
			}
		case <-deadline:
			t.Fatalf("WatchPresence: no Up for test-b within 2s")
		}
	}
	if !gotUp {
		t.Fatalf("WatchPresence: did not observe test-b Up")
	}

	if err := cB.Close(); err != nil {
		t.Fatalf("Close B: %v", err)
	}

	deadline = time.After(2 * time.Second)
	for {
		select {
		case e := <-events:
			pc, ok := e.(PresenceChange)
			if !ok {
				t.Fatalf("WatchPresence: got non-PresenceChange %T", e)
			}
			if pc.AgentID() == "test-b" && !pc.Up() {
				return
			}
		case <-deadline:
			t.Fatalf("WatchPresence: no Down for test-b within 2s")
		}
	}
}

func TestWatchPresence_CloserIdempotent(t *testing.T) {
	c := mustOpen(t)
	events, closer, err := c.WatchPresence(context.Background())
	if err != nil {
		t.Fatalf("WatchPresence: %v", err)
	}
	if err := closer(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := closer(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// Channel must be closed after the first closer call.
	select {
	case _, ok := <-events:
		if ok {
			t.Fatalf("WatchPresence: channel still open post-close")
		}
	case <-time.After(time.Second):
		t.Fatalf("WatchPresence: channel not closed within 1s")
	}
}

func TestWho_NilCtxPanics(t *testing.T) {
	c := mustOpen(t)
	requirePanic(t, func() {
		_, _ = c.Who(nilCtx)
	}, "ctx is nil")
}

func TestWatchPresence_NilCtxPanics(t *testing.T) {
	c := mustOpen(t)
	requirePanic(t, func() {
		_, _, _ = c.WatchPresence(nilCtx)
	}, "ctx is nil")
}
