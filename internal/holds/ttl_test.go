package holds_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/holds"
)

func TestTTL_ExpiryMakesWhoHasReturnFalse(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	file := "/work/ttl-a.txt"

	if err := m.Announce(ctx, file, newHold("A", 50*time.Millisecond)); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	got, ok, err := m.WhoHas(ctx, file)
	if err != nil {
		t.Fatalf("WhoHas: %v", err)
	}
	if ok {
		t.Fatalf(
			"WhoHas post-expiry: expected false, got %+v", got,
		)
	}
}

func TestTTL_AnnounceAfterExpiry_Succeeds_DifferentAgent(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	file := "/work/ttl-b.txt"

	if err := m.Announce(ctx, file, newHold("A", 50*time.Millisecond)); err != nil {
		t.Fatalf("Announce A: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	err := m.Announce(ctx, file, newHold("B", 50*time.Millisecond))
	if err != nil {
		t.Fatalf("Announce B: %v", err)
	}
	if errors.Is(err, holds.ErrHeldByAnother) {
		t.Fatalf("expected A's expired hold to yield to B")
	}
	got, ok, err := m.WhoHas(ctx, file)
	if err != nil || !ok || got.AgentID != "B" {
		t.Fatalf(
			"WhoHas: ok=%v agent=%q err=%v", ok, got.AgentID, err,
		)
	}
}
