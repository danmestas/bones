package presence

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// validConfig returns a fully-valid Config bound to nc. Tests override
// individual fields on the returned struct to exercise the validate-
// and open-time branches. HeartbeatInterval is 200ms by default so TTL
// is 600ms — short enough to exercise expiry in a test, long enough to
// stay above ticker jitter on slow CI runners.
func validConfig(nc *nats.Conn) Config {
	return Config{
		AgentID:           "bones-test0001",
		Project:           "bones",
		Bucket:            "bones-presence-test",
		NATSConn:          nc,
		HeartbeatInterval: 200 * time.Millisecond,
	}
}

func TestConfigValidate_Valid(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cfg := validConfig(nc)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestConfigValidate_Invalid(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	base := validConfig(nc)
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantKey string
	}{
		{
			name:    "empty AgentID",
			mutate:  func(c *Config) { c.AgentID = "" },
			wantKey: "AgentID",
		},
		{
			name:    "empty Project",
			mutate:  func(c *Config) { c.Project = "" },
			wantKey: "Project",
		},
		{
			name:    "empty Bucket",
			mutate:  func(c *Config) { c.Bucket = "" },
			wantKey: "Bucket",
		},
		{
			name:    "nil NATSConn",
			mutate:  func(c *Config) { c.NATSConn = nil },
			wantKey: "NATSConn",
		},
		{
			name:    "zero HeartbeatInterval",
			mutate:  func(c *Config) { c.HeartbeatInterval = 0 },
			wantKey: "HeartbeatInterval",
		},
		{
			name:    "negative ChanBuffer",
			mutate:  func(c *Config) { c.ChanBuffer = -1 },
			wantKey: "ChanBuffer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Fatalf(
					"error %q missing field %q",
					err.Error(), tc.wantKey,
				)
			}
		})
	}
}

func TestOpen_Valid(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m, err := Open(context.Background(), validConfig(nc))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if m == nil {
		t.Fatalf("Open returned nil Manager")
	}
}

func TestClose_Idempotent(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m, err := Open(context.Background(), validConfig(nc))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestWho_Self(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m, err := Open(context.Background(), validConfig(nc))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	entries, err := m.Who(context.Background())
	if err != nil {
		t.Fatalf("Who: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Who returned %d entries, want 1", len(entries))
	}
	if entries[0].AgentID != "bones-test0001" {
		t.Fatalf(
			"Who AgentID = %q, want bones-test0001",
			entries[0].AgentID,
		)
	}
	if entries[0].Project != "bones" {
		t.Fatalf(
			"Who Project = %q, want bones", entries[0].Project,
		)
	}
	if entries[0].LastSeen.IsZero() {
		t.Fatalf("Who LastSeen is zero")
	}
}

func TestWho_MultipleAgents(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cfgA := validConfig(nc)
	cfgA.AgentID = "bones-aaaa0001"
	cfgB := validConfig(nc)
	cfgB.AgentID = "bones-bbbb0001"

	mA, err := Open(context.Background(), cfgA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	t.Cleanup(func() { _ = mA.Close() })
	mB, err := Open(context.Background(), cfgB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	t.Cleanup(func() { _ = mB.Close() })

	entries, err := mA.Who(context.Background())
	if err != nil {
		t.Fatalf("Who: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Who returned %d entries, want 2", len(entries))
	}
	ids := map[string]bool{}
	for _, e := range entries {
		ids[e.AgentID] = true
	}
	if !ids["bones-aaaa0001"] || !ids["bones-bbbb0001"] {
		t.Fatalf("Who missing one or both agents: %+v", entries)
	}
}

func TestWho_ProjectScoped(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cfgA := validConfig(nc)
	cfgA.AgentID = "proj-a-aaaa0001"
	cfgA.Project = "proj-a"
	cfgB := validConfig(nc)
	cfgB.AgentID = "proj-b-bbbb0001"
	cfgB.Project = "proj-b"

	mA, err := Open(context.Background(), cfgA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	t.Cleanup(func() { _ = mA.Close() })
	mB, err := Open(context.Background(), cfgB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	t.Cleanup(func() { _ = mB.Close() })

	entries, err := mA.Who(context.Background())
	if err != nil {
		t.Fatalf("Who A: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("A's Who returned %d, want 1 (project scope)", len(entries))
	}
	if entries[0].Project != "proj-a" {
		t.Fatalf("A sees wrong project: %q", entries[0].Project)
	}
}

func TestPresent_SelfAndPeerAndMissing(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cfgA := validConfig(nc)
	cfgA.AgentID = "bones-prs10001"
	cfgB := validConfig(nc)
	cfgB.AgentID = "bones-prs20001"

	mA, err := Open(context.Background(), cfgA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	t.Cleanup(func() { _ = mA.Close() })
	mB, err := Open(context.Background(), cfgB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	t.Cleanup(func() { _ = mB.Close() })

	selfOK, err := mA.Present(context.Background(), cfgA.AgentID)
	if err != nil {
		t.Fatalf("Present self: %v", err)
	}
	if !selfOK {
		t.Fatalf("Present self: got false, want true")
	}
	peerOK, err := mA.Present(context.Background(), cfgB.AgentID)
	if err != nil {
		t.Fatalf("Present peer: %v", err)
	}
	if !peerOK {
		t.Fatalf("Present peer: got false, want true")
	}
	ghostOK, err := mA.Present(context.Background(), "bones-ghst0001")
	if err != nil {
		t.Fatalf("Present ghost: %v", err)
	}
	if ghostOK {
		t.Fatalf("Present ghost: got true, want false")
	}
}

func TestPresent_ProjectScoped(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cfgA := validConfig(nc)
	cfgA.AgentID = "proj-a-pres0001"
	cfgA.Project = "proj-a"
	cfgB := validConfig(nc)
	cfgB.AgentID = "proj-b-pres0001"
	cfgB.Project = "proj-b"

	mA, err := Open(context.Background(), cfgA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	t.Cleanup(func() { _ = mA.Close() })
	mB, err := Open(context.Background(), cfgB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	t.Cleanup(func() { _ = mB.Close() })

	// A is in proj-a; asking about B (in proj-b) must report not-present
	// because Present keys on the *caller's* project scope.
	crossOK, err := mA.Present(context.Background(), cfgB.AgentID)
	if err != nil {
		t.Fatalf("Present cross-project: %v", err)
	}
	if crossOK {
		t.Fatalf("Present cross-project: got true, want false (scope leak)")
	}
}

func TestPresent_ErrClosed(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	m, err := Open(context.Background(), validConfig(nc))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = m.Present(context.Background(), "any-agent")
	if err == nil {
		t.Fatalf("Present after Close: expected ErrClosed, got nil")
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Present after Close: err %v is not ErrClosed", err)
	}
}

func TestWatch_Up(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cfgA := validConfig(nc)
	cfgA.AgentID = "bones-watch001"
	mA, err := Open(context.Background(), cfgA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	t.Cleanup(func() { _ = mA.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, err := mA.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// B opening generates an Up event on A's watch.
	cfgB := validConfig(nc)
	cfgB.AgentID = "bones-watch002"
	mB, err := Open(context.Background(), cfgB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	t.Cleanup(func() { _ = mB.Close() })

	select {
	case evt := <-events:
		if evt.AgentID != "bones-watch002" {
			t.Fatalf("Watch: got AgentID %q, want bones-watch002", evt.AgentID)
		}
		if evt.Kind != EventUp {
			t.Fatalf("Watch: got Kind %v, want EventUp", evt.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Watch: no event within 2s")
	}
}

func TestWatch_Down(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cfgA := validConfig(nc)
	cfgA.AgentID = "bones-watch001"
	mA, err := Open(context.Background(), cfgA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	t.Cleanup(func() { _ = mA.Close() })

	cfgB := validConfig(nc)
	cfgB.AgentID = "bones-watch002"
	mB, err := Open(context.Background(), cfgB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, err := mA.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// B closing (explicit delete) generates a Down event on A's watch.
	if err := mB.Close(); err != nil {
		t.Fatalf("Close B: %v", err)
	}

	// Drain until we see the Down event for B. We may see one Up first
	// if B's periodic heartbeat fires between Watch start and Close;
	// the test is satisfied once a Down for watch002 arrives.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt.AgentID == "bones-watch002" && evt.Kind == EventDown {
				return
			}
		case <-deadline:
			t.Fatalf("Watch: no Down event for watch002 within 2s")
		}
	}
}
