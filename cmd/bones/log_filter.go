package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"
)

// suppressBenignSyncErrorHandler wraps another slog.Handler and drops
// known-benign "sync error" records emitted by EdgeSync/leaf's
// per-target sync loop (agent.go's `for _, target := range
// a.syncTargets`) when the target is "nats" and the error is the
// expected round-0 "no responders available" race.
//
// EdgeSync iterates each registered sync target independently and
// logs ERROR for any that fails. NATS subject-interest hasn't
// propagated through the leaf-node mesh on the first publish, so the
// NATS target reports "no responders" and the HTTP target succeeds
// in the same loop. Logging the NATS branch as ERROR creates noise
// on every commit. The HTTP branch carries the real result.
//
// Other NATS errors (auth failures, dropped connections, schema
// mismatches) still pass through. Non-"sync error" ERROR lines pass
// through unchanged. See bones #118 and the project-code commentary
// in internal/coord/leaf.go for the underlying mesh behavior.
type suppressBenignSyncErrorHandler struct {
	inner slog.Handler
}

func (h suppressBenignSyncErrorHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h suppressBenignSyncErrorHandler) Handle(ctx context.Context, r slog.Record) error {
	if isBenignNATSSyncError(r) {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h suppressBenignSyncErrorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return suppressBenignSyncErrorHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h suppressBenignSyncErrorHandler) WithGroup(name string) slog.Handler {
	return suppressBenignSyncErrorHandler{inner: h.inner.WithGroup(name)}
}

// isBenignNATSSyncError matches EdgeSync/leaf's `sync error` ERROR log
// when target=nats and the error wraps nats.ErrNoResponders or its
// rendered substring. The error attribute can be either the sentinel
// itself or a wrapped error whose String() contains "no responders" —
// libfossil's exchange-round error wrapping varies by call site, so
// we accept both shapes.
func isBenignNATSSyncError(r slog.Record) bool {
	if r.Level != slog.LevelError || r.Message != "sync error" {
		return false
	}
	var (
		isNATS    bool
		errSeen   error
		errString string
	)
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "target":
			if a.Value.String() == "nats" {
				isNATS = true
			}
		case "error":
			if e, ok := a.Value.Any().(error); ok {
				errSeen = e
			}
			errString = a.Value.String()
		}
		return true
	})
	if !isNATS {
		return false
	}
	if errSeen != nil && errors.Is(errSeen, nats.ErrNoResponders) {
		return true
	}
	return strings.Contains(errString, "no responders")
}
