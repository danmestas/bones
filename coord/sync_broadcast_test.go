package coord

import (
	"context"
	"encoding/json"
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
	defer sub.Unsubscribe()
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
