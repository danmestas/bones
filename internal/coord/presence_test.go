package coord

import (
	"context"
	"testing"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// presenceCfgForAgent returns a validConfigWithURL tweaked for a
// second agent on the same substrate: new AgentID and a namespaced
// per ADR 0047 chat lives on a workspace-shared JetStream stream, so
// per-agent Coords share the chat stream without colliding.
func presenceCfgForAgent(
	t *testing.T, url, agentID string,
) Config {
	t.Helper()
	cfg := validConfigWithURL(t, url)
	cfg.AgentID = agentID
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

func TestWho_NilCtxPanics(t *testing.T) {
	c := mustOpen(t)
	requirePanic(t, func() {
		_, _ = c.Who(nilCtx)
	}, "ctx is nil")
}
