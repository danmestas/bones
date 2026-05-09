package tasks_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
)

// TestLive_DeliversNewEvents asserts that an event published *after* a
// Live subscription opens lands on the channel within a short window.
// Pins the basic happy path: subscribe, mutate, receive.
func TestLive_DeliversNewEvents(t *testing.T) {
	m, _, cleanup := openEventLogManager(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := m.Live(ctx)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}

	rec := txNewTask("bones-live-1")
	if err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
		return tx.Create(rec)
	}); err != nil {
		t.Fatalf("Tx.Create: %v", err)
	}

	select {
	case env, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before event arrived")
		}
		if env.TaskID != rec.ID {
			t.Errorf("got TaskID %q, want %q", env.TaskID, rec.ID)
		}
		if env.Type != tasks.EventTypeCreated {
			t.Errorf("got Type %s, want EventTypeCreated", env.Type)
		}
		if env.Timestamp.IsZero() {
			t.Error("envelope Timestamp is zero — Live must preserve the original event time")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for live event delivery")
	}
}

// TestLive_PreSubscribeEventsNotDelivered pins the DeliverNewPolicy
// semantic: events published *before* Live subscribes are NOT delivered
// to that subscriber. Replay covers backfill; Live is for new events
// only.
func TestLive_PreSubscribeEventsNotDelivered(t *testing.T) {
	m, _, cleanup := openEventLogManager(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Mutate before subscribing.
	pre := txNewTask("bones-live-pre")
	if err := m.Tx(ctx, pre.ID, func(tx *tasks.Tx) error {
		return tx.Create(pre)
	}); err != nil {
		t.Fatalf("Tx.Create pre: %v", err)
	}

	ch, err := m.Live(ctx)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}

	// Mutate after subscribing.
	post := txNewTask("bones-live-post")
	if err := m.Tx(ctx, post.ID, func(tx *tasks.Tx) error {
		return tx.Create(post)
	}); err != nil {
		t.Fatalf("Tx.Create post: %v", err)
	}

	// Drain for a short window. Only the post-subscribe event is allowed.
	deadline := time.After(1500 * time.Millisecond)
	seen := make(map[string]bool)
loop:
	for {
		select {
		case env, ok := <-ch:
			if !ok {
				break loop
			}
			seen[env.TaskID] = true
		case <-deadline:
			break loop
		}
	}

	if seen[pre.ID] {
		t.Errorf("Live delivered pre-subscribe event for %q (DeliverNewPolicy violated)", pre.ID)
	}
	if !seen[post.ID] {
		t.Errorf("Live did not deliver post-subscribe event for %q", post.ID)
	}
}

// TestLive_StopsOnCtxCancel pins that canceling ctx closes the channel
// promptly so callers see an end-of-stream signal rather than blocking.
func TestLive_StopsOnCtxCancel(t *testing.T) {
	m, _, cleanup := openEventLogManager(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := m.Live(ctx)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}

	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			// A late event arriving before cleanup is fine; just keep
			// draining until the channel closes.
			select {
			case _, ok2 := <-ch:
				if ok2 {
					t.Error("channel still open after multiple drain attempts post-cancel")
				}
			case <-time.After(2 * time.Second):
				t.Error("timed out waiting for channel close after ctx cancel")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for channel close after ctx cancel")
	}
}

// TestLive_ErrorWhenEventLogDisabled pins the contract that Live
// returns an error rather than a nil channel when the manager wasn't
// opened with EnableEventLog. openTestManager (in subscribe_test.go)
// opens the KV-only mode used by tests that don't need the log.
func TestLive_ErrorWhenEventLogDisabled(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()

	_, err := m.Live(context.Background())
	if err == nil {
		t.Fatal("Live with event log disabled should return error")
	}
	if !strings.Contains(err.Error(), "event log disabled") {
		t.Errorf("error %q should mention event log disabled", err.Error())
	}
}
