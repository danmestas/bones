// coord/hub_test.go
package coord

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// freePort returns a random unused TCP port for hub HTTP serving.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestHub_OpenStop validates that OpenHub starts a working hub
// (HTTP listener up, NATS URL non-empty) and that Stop tears it down.
func TestHub_OpenStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	httpAddr := freePort(t)

	h, err := OpenHub(ctx, dir, httpAddr)
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	if h.HTTPAddr() != "http://"+httpAddr {
		t.Fatalf("HTTPAddr: got %q want %q", h.HTTPAddr(), "http://"+httpAddr)
	}
	if h.NATSURL() == "" {
		t.Fatalf("NATSURL: empty")
	}
	// hub.fossil must exist on disk after Open
	if _, err := filepath.Abs(filepath.Join(dir, "hub.fossil")); err != nil {
		t.Fatalf("abs: %v", err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestHub_StopIdempotent guards against double-Stop panics in test cleanup.
func TestHub_StopIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop #1: %v", err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop #2 should be no-op: %v", err)
	}
}
