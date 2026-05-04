package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/testutil/natstest"
)

// newAutoclaimCoord opens a real coord.Coord against the per-test
// embedded NATS server. Per ADR 0030 the autoclaim path is covered
// by real-substrate tests; mocks would hide JetStream CAS races
// (TestRunAutoclaim_ClaimRace_ReturnsRaceLost, below).
func newAutoclaimCoord(t *testing.T, agentID string) *coord.Coord {
	t.Helper()
	nc, _ := natstest.NewJetStreamServer(t)
	return newAutoclaimCoordOnURL(t, nc.ConnectedUrl(), agentID)
}

func newAutoclaimCoordOnURL(t *testing.T, url, agentID string) *coord.Coord {
	t.Helper()
	cfg := coord.Config{
		AgentID:      agentID,
		NATSURL:      url,
		CheckoutRoot: filepath.Join(t.TempDir(), agentID+"-checkouts"),
	}
	c, err := coord.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open(%s): %v", agentID, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRunAutoclaim_Disabled_NoOp(t *testing.T) {
	c := newAutoclaimCoord(t, "agent-autoclaim")
	ctx := context.Background()

	opts := autoclaimOpts{Enabled: false, Idle: true, ClaimTTL: time.Minute}
	res, err := runAutoclaimTick(ctx, c, opts)
	if err != nil {
		t.Fatalf("runAutoclaimTick: %v", err)
	}
	if res.Action != autoclaimDisabled {
		t.Fatalf("Action=%q, want %q", res.Action, autoclaimDisabled)
	}
}

func TestRunAutoclaim_Busy_NoOp(t *testing.T) {
	c := newAutoclaimCoord(t, "agent-autoclaim")
	ctx := context.Background()

	opts := autoclaimOpts{Enabled: true, Idle: false, ClaimTTL: time.Minute}
	res, err := runAutoclaimTick(ctx, c, opts)
	if err != nil {
		t.Fatalf("runAutoclaimTick: %v", err)
	}
	if res.Action != autoclaimBusy {
		t.Fatalf("Action=%q, want %q", res.Action, autoclaimBusy)
	}
}

func TestRunAutoclaim_ClaimedTaskOwnedByAgent_NoOp(t *testing.T) {
	c := newAutoclaimCoord(t, "agent-autoclaim")
	ctx := context.Background()
	id, err := c.OpenTask(ctx, "owned", []string{"/work/owned.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := c.Claim(ctx, id, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	defer func() { _ = rel() }()

	opts := autoclaimOpts{Enabled: true, Idle: true, ClaimTTL: time.Minute}
	res, err := runAutoclaimTick(ctx, c, opts)
	if err != nil {
		t.Fatalf("runAutoclaimTick: %v", err)
	}
	if res.Action != autoclaimAlreadyClaimed {
		t.Fatalf("Action=%q, want %q", res.Action, autoclaimAlreadyClaimed)
	}
}

func TestRunAutoclaim_NoReady_NoOp(t *testing.T) {
	c := newAutoclaimCoord(t, "agent-autoclaim")
	ctx := context.Background()

	opts := autoclaimOpts{Enabled: true, Idle: true, ClaimTTL: time.Minute}
	res, err := runAutoclaimTick(ctx, c, opts)
	if err != nil {
		t.Fatalf("runAutoclaimTick: %v", err)
	}
	if res.Action != autoclaimNoReady {
		t.Fatalf("Action=%q, want %q", res.Action, autoclaimNoReady)
	}
}

func TestRunAutoclaim_ClaimsOldestReadyTask(t *testing.T) {
	c := newAutoclaimCoord(t, "agent-autoclaim")
	ctx := context.Background()

	first, err := c.OpenTask(ctx, "first", []string{"/work/first.go"})
	if err != nil {
		t.Fatalf("OpenTask(first): %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	second, err := c.OpenTask(ctx, "second", []string{"/work/second.go"})
	if err != nil {
		t.Fatalf("OpenTask(second): %v", err)
	}

	opts := autoclaimOpts{Enabled: true, Idle: true, ClaimTTL: time.Minute}
	res, err := runAutoclaimTick(ctx, c, opts)
	if err != nil {
		t.Fatalf("runAutoclaimTick: %v", err)
	}
	if res.Action != autoclaimClaimed {
		t.Fatalf("Action=%q, want %q", res.Action, autoclaimClaimed)
	}
	if res.TaskID != first {
		t.Fatalf("TaskID=%q, want %q", res.TaskID, first)
	}

	prime, err := c.Prime(ctx)
	if err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if len(prime.ClaimedTasks) != 1 || prime.ClaimedTasks[0].ID() != first {
		t.Fatalf("claimed tasks = %#v, want only %q", prime.ClaimedTasks, first)
	}
	if second == first {
		t.Fatalf("expected distinct tasks")
	}
}

func TestRunAutoclaim_ClaimRace_ReturnsRaceLost(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cA := newAutoclaimCoordOnURL(t, nc.ConnectedUrl(), "agent-A")
	cB := newAutoclaimCoordOnURL(t, nc.ConnectedUrl(), "agent-B")
	ctx := context.Background()

	id, err := cA.OpenTask(ctx, "shared", []string{"/work/shared.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}

	type tickResult struct {
		res autoclaimResult
		err error
	}
	opts := autoclaimOpts{Enabled: true, Idle: true, ClaimTTL: time.Minute}
	results := make(chan tickResult, 2)
	go func() {
		res, err := runAutoclaimTick(ctx, cA, opts)
		results <- tickResult{res: res, err: err}
	}()
	go func() {
		res, err := runAutoclaimTick(ctx, cB, opts)
		results <- tickResult{res: res, err: err}
	}()

	first := <-results
	second := <-results
	if first.err != nil {
		t.Fatalf("first runAutoclaimTick: %v", first.err)
	}
	if second.err != nil {
		t.Fatalf("second runAutoclaimTick: %v", second.err)
	}

	var sawClaimed, sawLost bool
	for _, got := range []autoclaimResult{first.res, second.res} {
		switch got.Action {
		case autoclaimClaimed:
			sawClaimed = true
			if got.TaskID != id {
				t.Fatalf("claimed TaskID=%q, want %q", got.TaskID, id)
			}
		case autoclaimRaceLost:
			sawLost = true
			if got.TaskID != id {
				t.Fatalf("race-lost TaskID=%q, want %q", got.TaskID, id)
			}
		case autoclaimNoReady:
			// The loser ran list-ready after the winner's claim
			// landed and observed no claimable tasks. From the
			// caller's perspective indistinguishable from race-lost.
			sawLost = true
		default:
			t.Fatalf("unexpected action %q", got.Action)
		}
	}
	if !sawClaimed || !sawLost {
		t.Fatalf(
			"want one claimed and one race-lost or no-ready, got %#v and %#v",
			first.res, second.res,
		)
	}
}

func TestRunAutoclaim_ClaimSuccess_PostsNoticeToTaskThread(t *testing.T) {
	c := newAutoclaimCoord(t, "agent-autoclaim")
	ctx := context.Background()
	id, err := c.OpenTask(ctx, "notice", []string{"/work/notice.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	events, closeSub, err := c.Subscribe(ctx, string(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = closeSub() }()

	res, err := runAutoclaimTick(ctx, c, autoclaimOpts{
		Enabled:  true,
		Idle:     true,
		ClaimTTL: time.Minute,
		AgentID:  "agent-autoclaim",
	})
	if err != nil {
		t.Fatalf("runAutoclaimTick: %v", err)
	}
	if res.Action != autoclaimClaimed {
		t.Fatalf("Action=%q, want %q", res.Action, autoclaimClaimed)
	}

	select {
	case ev := <-events:
		msg, ok := ev.(coord.ChatMessage)
		if !ok {
			t.Fatalf("event type = %T, want coord.ChatMessage", ev)
		}
		want := "claimed by agent-autoclaim"
		if msg.Body() != want {
			t.Fatalf("body=%q, want %q", msg.Body(), want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for claim notice")
	}
}
