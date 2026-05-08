package hub

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/bones/internal/tasks"
)

// startRPCProjector wires a hub-side consumer of `tasks.events.>` so
// every state-mutating task event observed on the JetStream stream
// produces one INFO entry in hub.log per #322. The projector is the
// concrete realization of "RPC log middleware on the hub side": the
// hub does not own RPC handlers (CLI verbs do, against JetStream
// substrate), so the equivalent on the hub side is to project the
// post-mutation event flow into the operator-facing log.
//
// Why subscribe to the event log rather than tap each CLI handler:
//
//   - Architecturally honest. The hub is a process boundary; CLI
//     verbs are different processes. The only thing the hub can
//     genuinely observe is what crosses NATS or Fossil.
//
//   - Single source of truth. Every Tx mutation publishes one event
//     to the stream (ADR 0052). Subscribing once captures the
//     complete mutation surface — no chance of a CLI verb forgetting
//     to call a logging hook.
//
// The projector is a best-effort goroutine: connect failures degrade
// to "no RPC log entries for this hub start", and the lifecycle
// errors land in hub.log via the existing Warnf path so an operator
// can see why entries are missing. Hub start does not block on
// projector readiness.
//
// Returns a stop func; callers defer it to clean up the consumer
// and NATS connection on hub teardown.
func startRPCProjector(
	ctx context.Context, natsURL string, hl *hubLogger,
) func() {
	// nopStop is the no-op cleanup returned on degraded paths so the
	// caller's defer is unconditional.
	nopStop := func() {}

	if hl == nil {
		return nopStop
	}

	nc, err := nats.Connect(natsURL,
		nats.Timeout(5*time.Second),
		nats.RetryOnFailedConnect(false),
	)
	if err != nil {
		hl.Warnf("hub: rpc-log projector skipped (nats dial: %v)", err)
		return nopStop
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		hl.Warnf("hub: rpc-log projector skipped (jetstream: %v)", err)
		return nopStop
	}

	stream, err := js.Stream(ctx, tasks.EventStreamName)
	if err != nil {
		nc.Close()
		hl.Warnf("hub: rpc-log projector skipped (stream: %v)", err)
		return nopStop
	}

	// DeliverNewPolicy: only entries published AFTER subscription
	// land in hub.log. Replay of historic events is a different
	// concern (the recovery loop already drains them); we do not
	// want hub.log to flood with hours of backfill on hub restart.
	cfg := jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{tasks.AllEventsSubject},
		DeliverPolicy:  jetstream.DeliverNewPolicy,
	}
	cons, err := stream.OrderedConsumer(ctx, cfg)
	if err != nil {
		nc.Close()
		hl.Warnf("hub: rpc-log projector skipped (consumer: %v)", err)
		return nopStop
	}

	_, cancel := context.WithCancel(ctx)
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		projectOne(hl, msg.Data())
	})
	if err != nil {
		cancel()
		nc.Close()
		hl.Warnf("hub: rpc-log projector skipped (consume: %v)", err)
		return nopStop
	}

	stopOnce := false
	return func() {
		if stopOnce {
			return
		}
		stopOnce = true
		cc.Stop()
		cancel()
		nc.Close()
	}
}

// projectOne decodes one stream message and writes the matching
// hub.log entry. Decode failures are swallowed — they would be
// caught by the tasks package's own validation, and propagating
// them out of the projector would just spam hub.log with duplicate
// noise.
//
// The agent field is sourced from the typed payload when present
// (Claimed, Closed) and falls back to "system" otherwise. ADR 0052's
// payload structs already carry the agent ID for the events where
// it's known; for Created / Updated / Linked / SlotChanged the
// originating identity is not stamped on the envelope, so "system"
// is the honest answer.
func projectOne(hl *hubLogger, raw []byte) {
	env, err := tasks.UnmarshalEnvelope(raw)
	if err != nil {
		return
	}
	if !env.Type.Valid() {
		return
	}
	rpc := rpcNameFromEventType(env.Type.String())
	agent := agentFromPayload(env)
	hl.Log(LogEntry{
		Level: selectLevel(rpc, nil),
		Event: EventRPC,
		RPC:   rpc,
		Agent: agent,
		Task:  env.TaskID,
	})
	_ = json.RawMessage(env.Payload)
}

// agentFromPayload extracts the agent identity from typed payloads
// where it's stamped. Returns "system" when unknown — the convention
// in #322's brief for hub-internal calls and for events whose
// originator is not carried on the envelope.
func agentFromPayload(env tasks.EventEnvelope) string {
	dec, err := tasks.DecodePayload(env)
	if err != nil {
		return "system"
	}
	switch p := dec.(type) {
	case tasks.ClaimedPayload:
		if p.AgentID != "" {
			return p.AgentID
		}
	case tasks.ClosedPayload:
		if p.AgentID != "" {
			return p.AgentID
		}
	}
	return "system"
}
