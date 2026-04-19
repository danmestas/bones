// Package natstest provides an embedded NATS server fixture for tests.
//
// Future hold/coord tests consume this instead of requiring an external
// NATS server. The fixture binds to a random loopback port so parallel
// tests do not collide, and teardown is idempotent.
//
// Usage:
//
//	func TestSomething(t *testing.T) {
//	    nc, cleanup := natstest.NewTestServer(t)
//	    defer cleanup()
//	    // nc is a connected *nats.Conn on a random port.
//	}
//
// The default fixture runs without JetStream. Tests that need the KV
// substrate (holds/coord) should call NewJetStreamServer instead.
package natstest

import (
	"os"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// readyTimeout bounds how long NewTestServer waits for the embedded
// nats-server to accept connections. EdgeSync's leaf agent uses the
// same 5s budget (see reference/EdgeSync/leaf/agent/nats_mesh.go).
const readyTimeout = 5 * time.Second

// NewTestServer starts an in-process nats-server on a random loopback
// port and returns a client connection plus a cleanup func. The cleanup
// is also registered via t.Cleanup, so `defer cleanup()` is optional.
// Calling cleanup more than once is a no-op.
//
// On any setup failure, NewTestServer calls t.Fatalf and does not return.
func NewTestServer(t *testing.T) (*nats.Conn, func()) {
	t.Helper()
	return newServer(t, false)
}

// NewJetStreamServer starts an in-process nats-server with JetStream
// enabled, storing state in a temporary directory that is removed when
// cleanup runs. Use this for tests that exercise JetStream KV.
//
// The returned cleanup is the single teardown point for connection,
// server, and store directory. Calling it more than once is a no-op.
func NewJetStreamServer(t *testing.T) (*nats.Conn, func()) {
	t.Helper()
	return newServer(t, true)
}

// newServer is the shared implementation behind NewTestServer and
// NewJetStreamServer. When jetStream is true, a transient StoreDir is
// created and wired into cleanup.
func newServer(t *testing.T, jetStream bool) (*nats.Conn, func()) {
	t.Helper()

	// NoLog short-circuits ConfigureLogger so the server starts with a nil
	// logger and writes nothing to stderr. NoSigs skips the signal handler
	// goroutine that a daemon needs but a test does not.
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: jetStream,
	}

	var storeDir string
	if jetStream {
		dir, err := os.MkdirTemp("", "natstest-js-*")
		if err != nil {
			t.Fatalf("natstest: create store dir: %v", err)
		}
		storeDir = dir
		opts.StoreDir = dir
	}

	s, err := natsserver.NewServer(opts)
	if err != nil {
		if storeDir != "" {
			_ = os.RemoveAll(storeDir)
		}
		t.Fatalf("natstest: create server: %v", err)
	}

	go s.Start()
	if !s.ReadyForConnections(readyTimeout) {
		s.Shutdown()
		s.WaitForShutdown()
		if storeDir != "" {
			_ = os.RemoveAll(storeDir)
		}
		t.Fatalf("natstest: server not ready within %s", readyTimeout)
	}

	nc, err := nats.Connect(s.ClientURL(), nats.Timeout(readyTimeout))
	if err != nil {
		s.Shutdown()
		s.WaitForShutdown()
		if storeDir != "" {
			_ = os.RemoveAll(storeDir)
		}
		t.Fatalf("natstest: connect to %s: %v", s.ClientURL(), err)
	}

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			nc.Close()
			s.Shutdown()
			s.WaitForShutdown()
			if storeDir != "" {
				_ = os.RemoveAll(storeDir)
			}
		})
	}
	t.Cleanup(cleanup)
	return nc, cleanup
}
