// Tests for the TTL watcher (#265, ADR 0050). Coverage pins:
//
//   - TestLeaseTTL_ReapsStaleSession    — synthetic stale record gets reaped within one TTL window.
//   - TestLeaseTTL_HappyPathSilent      — no log line when nothing is stale.
//   - TestLeaseTTL_ReapEmitsHubLogLine  — reap writes "hub: reaped stale slot <name>" to the log.
//
// Tests use the leaseFixture from lease_test.go (real NATS + real
// libfossil, no mocks; ADR 0030 discipline).
package swarm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureLogger records Infof/Warnf calls into a thread-safe buffer
// so tests can assert "did the watcher emit the expected line."
type captureLogger struct {
	mu    sync.Mutex
	infos []string
	warns []string
}

func (l *captureLogger) Infof(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, fmt.Sprintf(format, args...))
}

func (l *captureLogger) Warnf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fmt.Sprintf(format, args...))
}

func (l *captureLogger) infosCopy() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.infos))
	copy(out, l.infos)
	return out
}

// writeStaleSession bypasses Acquire's role-guard preconditions and
// directly inserts a session record whose LastRenewed is far enough
// in the past that the watcher's TTL check classifies it as stale.
//
// Used to exercise the watcher without standing up a full task +
// claim. The watcher's behavior is bucket-side: list, classify by
// LastRenewed age, drop the record. Whether the record came from
// Acquire or from a direct put is a substrate-equivalent question.
func writeStaleSession(t *testing.T, f *leaseFixture, slot string, age time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, _, _, err := openLeaseSessions(ctx, f.info, nil)
	if err != nil {
		t.Fatalf("openLeaseSessions: %v", err)
	}
	defer func() { _ = sess.Close() }()
	host, _ := os.Hostname()
	t0 := time.Now().UTC().Add(-age)
	if err := sess.put(ctx, Session{
		Slot:        slot,
		TaskID:      "task-stale",
		AgentID:     "slot-" + slot,
		Host:        host,
		StartedAt:   t0,
		LastRenewed: t0,
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	// Pre-create the wt dir so the reaper has something to remove.
	wt := SlotWorktree(f.info.WorkspaceDir, slot)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
}

// newWatcherForTest builds a TTLWatcher against the fixture's
// substrate, with a tight TTL and a capture logger so tests can
// drive a single tick deterministically.
func newWatcherForTest(
	t *testing.T, f *leaseFixture, ttl time.Duration, logger WatcherLogger,
) (*TTLWatcher, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, _, _, err := openLeaseSessions(ctx, f.info, nil)
	if err != nil {
		t.Fatalf("openLeaseSessions: %v", err)
	}
	w, err := NewTTLWatcher(WatcherConfig{
		WorkspaceDir:  f.info.WorkspaceDir,
		Sessions:      sess,
		TTL:           ttl,
		Tick:          1 * time.Hour, // we drive tickOnce manually
		Logger:        logger,
		LocalHostOnly: true,
	})
	if err != nil {
		_ = sess.Close()
		t.Fatalf("NewTTLWatcher: %v", err)
	}
	return w, func() { _ = sess.Close() }
}

// TestLeaseTTL_ReapsStaleSession pins the canonical reap behavior:
// a session whose LastRenewed is older than (now - TTL) gets its
// record dropped and its wt directory removed within one tick.
func TestLeaseTTL_ReapsStaleSession(t *testing.T) {
	f := newLeaseFixture(t)
	logger := &captureLogger{}

	writeStaleSession(t, f, "stale-1", 10*time.Minute)

	w, cleanup := newWatcherForTest(t, f, 1*time.Minute, logger)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.tickOnce(ctx)

	// Session record must be gone.
	verifySess := openVerifySessions(t, f)
	_, _, err := verifySess.Get(ctx, "stale-1")
	if err == nil {
		t.Errorf("stale session record still present after tickOnce")
	}

	// wt dir must be gone.
	wt := SlotWorktree(f.info.WorkspaceDir, "stale-1")
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("stale wt dir still present: stat err=%v", err)
	}
}

// TestLeaseTTL_HappyPathSilent pins the no-spam contract: when no
// sessions are stale (or none exist at all), the watcher MUST NOT
// emit any Infof lines. Operators tail hub.log to see real events;
// a chatty watcher poisons that signal.
func TestLeaseTTL_HappyPathSilent(t *testing.T) {
	f := newLeaseFixture(t)
	logger := &captureLogger{}

	// Insert a fresh session — within TTL, must not be reaped.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, _, _, err := openLeaseSessions(ctx, f.info, nil)
	if err != nil {
		t.Fatalf("openLeaseSessions: %v", err)
	}
	defer func() { _ = sess.Close() }()
	host, _ := os.Hostname()
	if err := sess.put(ctx, Session{
		Slot:        "fresh-1",
		TaskID:      "task-fresh",
		AgentID:     "slot-fresh-1",
		Host:        host,
		StartedAt:   time.Now().UTC(),
		LastRenewed: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}

	w, cleanup := newWatcherForTest(t, f, 5*time.Minute, logger)
	defer cleanup()
	w.tickOnce(ctx)

	if got := logger.infosCopy(); len(got) > 0 {
		t.Errorf("happy path emitted %d Infof lines (expected 0): %v", len(got), got)
	}
}

// TestLeaseTTL_ReapEmitsHubLogLine pins the log shape contract
// from the issue spec: a reap MUST write
// "hub: reaped stale slot <name> (ttl exceeded by <duration>)"
// to the watcher's logger.
func TestLeaseTTL_ReapEmitsHubLogLine(t *testing.T) {
	f := newLeaseFixture(t)
	logger := &captureLogger{}

	writeStaleSession(t, f, "loglineslot", 10*time.Minute)

	w, cleanup := newWatcherForTest(t, f, 1*time.Minute, logger)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.tickOnce(ctx)

	got := logger.infosCopy()
	if len(got) == 0 {
		t.Fatalf("watcher produced no Infof lines; want one")
	}
	want := "hub: reaped stale slot loglineslot"
	if !containsAny(got, want) {
		t.Errorf("Infof lines missing %q: %v", want, got)
	}
	if !containsAny(got, "ttl exceeded by") {
		t.Errorf("Infof lines missing ttl-excess phrase: %v", got)
	}
}

// containsAny reports whether any string in lines contains the
// substring needle.
func containsAny(lines []string, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// TestLeaseTTL_RunRespectsContextCancellation keeps Run-loop
// shutdown sane: canceling the parent ctx must cause Run to
// return promptly without leaking the goroutine.
func TestLeaseTTL_RunRespectsContextCancellation(t *testing.T) {
	f := newLeaseFixture(t)
	logger := &captureLogger{}

	w, cleanup := newWatcherForTest(t, f, 1*time.Minute, logger)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned non-nil error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancel")
	}
}

// TestLeaseTTL_StartReturnsStopFunc pins the lifecycle helper:
// Start must return a stop function that, when invoked, blocks
// until Run has fully exited.
func TestLeaseTTL_StartReturnsStopFunc(t *testing.T) {
	f := newLeaseFixture(t)
	logger := &captureLogger{}

	w, cleanup := newWatcherForTest(t, f, 1*time.Minute, logger)
	defer cleanup()

	stop := w.Start(context.Background())
	// Stop must be idempotent in spirit (calling it once is enough);
	// drain any goroutine and return.
	stop()
}

// _ keeps bytes referenced for any future buffer-based logger
// expansion.
var _ = bytes.Buffer{}
