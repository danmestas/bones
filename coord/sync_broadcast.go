package coord

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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

// tipSubscriber consumes coord.tip.changed broadcasts and runs pullFn
// when the broadcast hash differs from the local tip (returned by
// localFn). Idempotent: identical hashes are no-ops. Closing unsubs.
type tipSubscriber struct {
	nc      *nats.Conn
	hubURL  string
	pullFn  func(ctx context.Context, hubURL string) error
	localFn func(ctx context.Context) (string, error)
	js      nats.JetStreamContext
	sub     *nats.Subscription
}

// Start declares a JetStream durable consumer named "coord-tip-<random>"
// and begins delivering messages to the handler. The durable name keeps
// missed broadcasts in JetStream's storage between reconnects per ADR
// edge-case 3.
func (s *tipSubscriber) Start(ctx context.Context) error {
	if ctx == nil {
		panic("coord.tipSubscriber.Start: ctx is nil")
	}
	if s.nc == nil || s.pullFn == nil || s.localFn == nil {
		panic("coord.tipSubscriber.Start: nil dependency")
	}
	js, err := s.nc.JetStream()
	if err != nil {
		return fmt.Errorf("coord.tipSubscriber: jetstream: %w", err)
	}
	s.js = js
	_, _ = js.AddStream(&nats.StreamConfig{
		Name:     "COORD_TIP",
		Subjects: []string{tipChangedSubject},
		Storage:  nats.FileStorage,
		MaxAge:   0,
	})
	durable := fmt.Sprintf("coord-tip-%d", nowNano())
	sub, err := js.Subscribe(tipChangedSubject, func(m *nats.Msg) {
		s.handle(ctx, m)
	}, nats.Durable(durable), nats.DeliverNew(), nats.AckExplicit())
	if err != nil {
		return fmt.Errorf("coord.tipSubscriber: subscribe: %w", err)
	}
	s.sub = sub
	return nil
}

// Close unsubscribes. Safe to call once.
func (s *tipSubscriber) Close() {
	if s.sub != nil {
		_ = s.sub.Unsubscribe()
		s.sub = nil
	}
}

func (s *tipSubscriber) handle(ctx context.Context, m *nats.Msg) {
	defer func() { _ = m.Ack() }()
	var p tipChangedPayload
	if err := json.Unmarshal(m.Data, &p); err != nil {
		return
	}
	local, err := s.localFn(ctx)
	if err != nil || local == p.ManifestHash {
		return
	}
	_ = s.pullFn(ctx, s.hubURL)
}

// nowNano is overridable in tests via build tags; default is time.Now.
var nowNano = func() int64 { return time.Now().UnixNano() }
