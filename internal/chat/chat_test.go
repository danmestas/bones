package chat_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/agent-infra/internal/chat"
	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// validConfig returns a fully-valid chat.Config bound to the given
// NATS URL and a fresh repo path under t.TempDir. Tests mutate the
// returned value to exercise specific Validate branches.
func validConfig(t *testing.T) chat.Config {
	t.Helper()
	return chat.Config{
		AgentID:        "agent-infra-testabcd",
		ProjectPrefix:  "agent-infra",
		Nats:           nil, // filled in by caller
		FossilRepoPath: filepath.Join(t.TempDir(), "chat.fossil"),
		MaxSubscribers: 32,
	}
}

// requirePanic verifies that fn panics with a message that contains
// want. Mirrors the helper in internal/tasks and internal/holds tests.
func requirePanic(t *testing.T, fn func(), want string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", want)
		}
		if !strings.Contains(fmt.Sprint(r), want) {
			t.Fatalf("panic %q does not contain %q", r, want)
		}
	}()
	fn()
}

func TestOpen_Valid(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc

	m, err := chat.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if m == nil {
		t.Fatalf("Open: returned nil Manager with nil error")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}
}

func TestOpen_InvalidConfig(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cases := []struct {
		name    string
		mutate  func(*chat.Config)
		wantKey string
	}{
		{
			name:    "empty AgentID",
			mutate:  func(c *chat.Config) { c.AgentID = "" },
			wantKey: "AgentID",
		},
		{
			name:    "empty ProjectPrefix",
			mutate:  func(c *chat.Config) { c.ProjectPrefix = "" },
			wantKey: "ProjectPrefix",
		},
		{
			name:    "nil Nats",
			mutate:  func(c *chat.Config) { c.Nats = nil },
			wantKey: "Nats",
		},
		{
			name:    "empty FossilRepoPath",
			mutate:  func(c *chat.Config) { c.FossilRepoPath = "" },
			wantKey: "FossilRepoPath",
		},
		{
			name:    "zero MaxSubscribers",
			mutate:  func(c *chat.Config) { c.MaxSubscribers = 0 },
			wantKey: "MaxSubscribers",
		},
		{
			name:    "negative MaxSubscribers",
			mutate:  func(c *chat.Config) { c.MaxSubscribers = -1 },
			wantKey: "MaxSubscribers",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Nats = nc
			tc.mutate(&cfg)
			m, err := chat.Open(context.Background(), cfg)
			if err == nil {
				t.Fatalf("Open: expected error for %s", tc.name)
			}
			if m != nil {
				t.Fatalf("Open: expected nil Manager on error")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Fatalf(
					"Open: error %q should mention %q",
					err, tc.wantKey,
				)
			}
			if !strings.Contains(err.Error(), "chat.Open") {
				t.Fatalf("Open: error lacks wrap prefix: %v", err)
			}
		})
	}
}

func TestOpen_NilCtxPanics(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc
	var nilCtx context.Context

	requirePanic(t, func() {
		_, _ = chat.Open(nilCtx, cfg)
	}, "ctx is nil")
}

func TestClose_Idempotent(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc

	m, err := chat.Open(context.Background(), cfg)
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
