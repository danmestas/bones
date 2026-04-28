package autoclaim

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/testutil/natstest"
)

func newTestCoord(t *testing.T, agentID string) *coord.Coord {
	t.Helper()
	nc, _ := natstest.NewJetStreamServer(t)
	return newCoordOnURL(t, nc.ConnectedUrl(), agentID)
}

func newCoordOnURL(t *testing.T, url, agentID string) *coord.Coord {
	t.Helper()
	cfg := coord.Config{
		AgentID:            agentID,
		NATSURL:            url,
		ChatFossilRepoPath: filepath.Join(t.TempDir(), agentID+"-chat.fossil"),
		CheckoutRoot:       filepath.Join(t.TempDir(), agentID+"-checkouts"),
	}
	c, err := coord.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open(%s): %v", agentID, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestTick_Disabled_NoOp(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
	ctx := context.Background()

	res, err := Tick(ctx, c, Options{Enabled: false, Idle: true, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionDisabled {
		t.Fatalf("Action=%q, want %q", res.Action, ActionDisabled)
	}
}

func TestTick_Busy_NoOp(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
	ctx := context.Background()

	res, err := Tick(ctx, c, Options{Enabled: true, Idle: false, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionBusy {
		t.Fatalf("Action=%q, want %q", res.Action, ActionBusy)
	}
}

func TestTick_ClaimedTaskOwnedByAgent_NoOp(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
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

	res, err := Tick(ctx, c, Options{Enabled: true, Idle: true, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionAlreadyClaimed {
		t.Fatalf("Action=%q, want %q", res.Action, ActionAlreadyClaimed)
	}
}

func TestTick_NoReady_NoOp(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
	ctx := context.Background()

	res, err := Tick(ctx, c, Options{Enabled: true, Idle: true, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionNoReady {
		t.Fatalf("Action=%q, want %q", res.Action, ActionNoReady)
	}
}

func TestTick_ClaimsOldestReadyTask(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
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

	res, err := Tick(ctx, c, Options{Enabled: true, Idle: true, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionClaimed {
		t.Fatalf("Action=%q, want %q", res.Action, ActionClaimed)
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

func TestTick_ClaimRace_ReturnsRaceLost(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cA := newCoordOnURL(t, nc.ConnectedUrl(), "agent-A")
	cB := newCoordOnURL(t, nc.ConnectedUrl(), "agent-B")
	ctx := context.Background()

	id, err := cA.OpenTask(ctx, "shared", []string{"/work/shared.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}

	type tickResult struct {
		res Result
		err error
	}
	results := make(chan tickResult, 2)
	go func() {
		res, err := Tick(ctx, cA, Options{Enabled: true, Idle: true, ClaimTTL: time.Minute})
		results <- tickResult{res: res, err: err}
	}()
	go func() {
		res, err := Tick(ctx, cB, Options{Enabled: true, Idle: true, ClaimTTL: time.Minute})
		results <- tickResult{res: res, err: err}
	}()

	first := <-results
	second := <-results
	if first.err != nil {
		t.Fatalf("first Tick: %v", first.err)
	}
	if second.err != nil {
		t.Fatalf("second Tick: %v", second.err)
	}

	var sawClaimed, sawRaceLost bool
	for _, got := range []Result{first.res, second.res} {
		switch got.Action {
		case ActionClaimed:
			sawClaimed = true
			if got.TaskID != id {
				t.Fatalf("claimed TaskID=%q, want %q", got.TaskID, id)
			}
		case ActionRaceLost:
			sawRaceLost = true
			if got.TaskID != id {
				t.Fatalf("race-lost TaskID=%q, want %q", got.TaskID, id)
			}
		default:
			t.Fatalf("unexpected action %q", got.Action)
		}
	}
	if !sawClaimed || !sawRaceLost {
		t.Fatalf("want one claimed and one race-lost, got %#v and %#v", first.res, second.res)
	}
}

func TestTick_ClaimSuccess_PostsNoticeToTaskThread(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
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

	res, err := Tick(ctx, c, Options{
		Enabled:  true,
		Idle:     true,
		ClaimTTL: time.Minute,
		AgentID:  "agent-autoclaim",
	})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionClaimed {
		t.Fatalf("Action=%q, want %q", res.Action, ActionClaimed)
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
