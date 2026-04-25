package coord

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// tipChangedSubject is the NATS subject coord uses for hub-tip change
// broadcasts. Single subject across all leaves; subscribers filter on
// payload.ManifestHash for idempotency.
const tipChangedSubject = "coord.tip.changed"

// tipChangedPayload is the on-the-wire JSON for tip.changed.
type tipChangedPayload struct {
	ManifestHash string `json:"manifest_hash"`
}

// publishTipChanged sends a tip.changed broadcast carrying manifestHash.
// OTel context (if any) is injected into NATS headers per ADR 0018.
func publishTipChanged(ctx context.Context, nc *nats.Conn, manifestHash string) error {
	if ctx == nil {
		panic("coord.publishTipChanged: ctx is nil")
	}
	if nc == nil {
		panic("coord.publishTipChanged: nc is nil")
	}
	if manifestHash == "" {
		panic("coord.publishTipChanged: manifestHash is empty")
	}
	body, err := json.Marshal(tipChangedPayload{ManifestHash: manifestHash})
	if err != nil {
		return fmt.Errorf("coord.publishTipChanged: marshal: %w", err)
	}
	msg := &nats.Msg{
		Subject: tipChangedSubject,
		Data:    body,
		Header:  nats.Header{},
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(msg.Header))
	if err := nc.PublishMsg(msg); err != nil {
		return fmt.Errorf("coord.publishTipChanged: publish: %w", err)
	}
	if err := nc.Flush(); err != nil {
		return fmt.Errorf("coord.publishTipChanged: flush: %w", err)
	}
	return nil
}
