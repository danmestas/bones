package coord

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

func TestSyncOnBroadcast_SpanOnPull(t *testing.T) {
	rec := installRecorder(t)
	ctx := context.Background()
	nc, _ := natstest.NewJetStreamServer(t)

	sub := &tipSubscriber{
		nc:      nc,
		hubURL:  "http://hub.example/",
		pullFn:  func(ctx context.Context, url string) error { return nil },
		localFn: func(ctx context.Context) (string, error) { return "old", nil },
	}
	defer sub.Close()
	if err := sub.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := publishTipChanged(ctx, nc, "new-hash"); err != nil {
		t.Fatalf("publishTipChanged: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	spans := rec.Spans("coord.SyncOnBroadcast")
	if len(spans) != 1 {
		t.Fatalf("expected 1 SyncOnBroadcast span, got %d", len(spans))
	}
	got := map[string]any{}
	for _, kv := range spans[0].Attributes {
		got[string(kv.Key)] = kv.Value.AsInterface()
	}
	if got["manifest.hash"] != "new-hash" {
		t.Errorf("manifest.hash: got %v want new-hash", got["manifest.hash"])
	}
	if got["pull.success"] != true {
		t.Errorf("pull.success: got %v want true", got["pull.success"])
	}
	if got["pull.skipped_idempotent"] != false {
		t.Errorf("pull.skipped_idempotent: got %v want false", got["pull.skipped_idempotent"])
	}
}

func TestSyncOnBroadcast_SkippedSpan(t *testing.T) {
	rec := installRecorder(t)
	ctx := context.Background()
	nc, _ := natstest.NewJetStreamServer(t)

	sub := &tipSubscriber{
		nc:     nc,
		hubURL: "http://hub.example/",
		pullFn: func(ctx context.Context, url string) error {
			t.Fatal("should not pull")
			return nil
		},
		localFn: func(ctx context.Context) (string, error) { return "same", nil },
	}
	defer sub.Close()
	if err := sub.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := publishTipChanged(ctx, nc, "same"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	spans := rec.Spans("coord.SyncOnBroadcast")
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	got := map[string]any{}
	for _, kv := range spans[0].Attributes {
		got[string(kv.Key)] = kv.Value.AsInterface()
	}
	if got["pull.skipped_idempotent"] != true {
		t.Errorf("pull.skipped_idempotent: got %v want true", got["pull.skipped_idempotent"])
	}
}
