package coord

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

func TestPublishTipChanged_PayloadShape(t *testing.T) {
	ctx := context.Background()
	nc, _ := natstest.NewJetStreamServer(t)

	got := make(chan *nats.Msg, 1)
	sub, err := nc.SubscribeSync("coord.tip.changed")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	go func() {
		m, _ := sub.NextMsg(2 * time.Second)
		got <- m
	}()

	if err := publishTipChanged(ctx, nc, "abc123def456"); err != nil {
		t.Fatalf("publishTipChanged: %v", err)
	}
	m := <-got
	if m == nil {
		t.Fatal("no broadcast received")
	}
	var payload struct {
		ManifestHash string `json:"manifest_hash"`
	}
	if err := json.Unmarshal(m.Data, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.ManifestHash != "abc123def456" {
		t.Fatalf("got hash %q, want abc123def456", payload.ManifestHash)
	}
}

func TestSubscriber_PullsOnBroadcast(t *testing.T) {
	ctx := context.Background()
	nc, _ := natstest.NewJetStreamServer(t)

	var calls atomic.Int64
	sub := &tipSubscriber{
		nc:      nc,
		hubURL:  "http://hub.example/",
		pullFn:  func(ctx context.Context, url string) error { calls.Add(1); return nil },
		localFn: func(ctx context.Context) (string, error) { return "old-hash", nil },
	}
	defer sub.Close()
	if err := sub.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := publishTipChanged(ctx, nc, "new-hash"); err != nil {
		t.Fatalf("publishTipChanged: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 pull call, got %d", got)
	}
}

func TestSubscriber_IdempotentOnSameHash(t *testing.T) {
	ctx := context.Background()
	nc, _ := natstest.NewJetStreamServer(t)

	var calls atomic.Int64
	sub := &tipSubscriber{
		nc:      nc,
		hubURL:  "http://hub.example/",
		pullFn:  func(ctx context.Context, url string) error { calls.Add(1); return nil },
		localFn: func(ctx context.Context) (string, error) { return "same-hash", nil },
	}
	defer sub.Close()
	if err := sub.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := publishTipChanged(ctx, nc, "same-hash"); err != nil {
		t.Fatalf("publishTipChanged: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("expected 0 pull calls (idempotent on same hash), got %d", got)
	}
}
