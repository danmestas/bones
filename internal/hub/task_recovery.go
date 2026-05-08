package hub

import (
	"context"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/tasks"
)

// recoveryDialTimeout bounds how long the recovery NATS connection
// will wait for the embedded server to accept a connect. The server
// has just been bound (we're called right after `hub: ready`) so the
// connect should be subsecond; this guards against a stuck listener
// surfacing as an indefinite hang during hub start.
const recoveryDialTimeout = 5 * time.Second

// runTaskRecovery dials the just-bound NATS server, opens a
// tasks.Manager with RecoverOnOpen=true so Recover drains
// synchronously inside Open, then closes everything.
//
// Per ADR 0052 §"Recovery": running Recover concurrently with live
// Tx writers is racy by design (the recovery loop and the writer
// both want to bring KV to parity with the stream and they CAS-
// clobber each other). Hub start is the one place where Recover is
// safe — it runs serially before any CLI verb can dial the new
// server, so no Tx is in flight. This helper is the wiring that
// makes the safety guarantee real: NATS up → recovery → CLI verbs
// allowed to connect.
//
// Errors are logged but do not fail hub start: a recovery failure
// does not block the workspace from coming up. The orphan events
// remain on the stream and the next bones-up will retry. The hub
// log carries the failure breadcrumb for `bones doctor`.
func runTaskRecovery(ctx context.Context, natsURL string, hl *hubLogger) {
	dialCtx, cancel := context.WithTimeout(ctx, recoveryDialTimeout)
	defer cancel()
	nc, err := nats.Connect(natsURL,
		nats.Timeout(recoveryDialTimeout),
		nats.RetryOnFailedConnect(false),
	)
	if err != nil {
		hl.Warnf("hub: recovery skipped (nats dial: %v)", err)
		return
	}
	defer nc.Close()
	mgr, err := tasks.Open(dialCtx, nc, tasks.Config{
		BucketName:     tasks.DefaultBucketName,
		HistoryDepth:   8,
		MaxValueSize:   64 * 1024,
		EnableEventLog: true,
		RecoverOnOpen:  true,
	})
	if err != nil {
		hl.Warnf("hub: recovery failed (%v) — orphan events will retry on next start", err)
		return
	}
	defer func() { _ = mgr.Close() }()
	hl.Infof("hub: task event-log recovery complete")
}
