package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestSuppressBenignNATSSyncError pins the fix for #118: a "sync error"
// log emitted by EdgeSync/leaf's per-target sync loop, when the target
// is "nats" and the error wraps nats.ErrNoResponders, must be dropped
// before reaching the underlying handler. The HTTP sync target
// succeeds independently in the same loop, so the NATS branch is
// known-benign noise on the round-0 race.
func TestSuppressBenignNATSSyncError(t *testing.T) {
	cases := []struct {
		name      string
		level     slog.Level
		msg       string
		attrs     []slog.Attr
		wantDrop  bool
		shouldSee string // substring expected when not dropped
	}{
		{
			name:  "drops nats no-responders sync error",
			level: slog.LevelError,
			msg:   "sync error",
			attrs: []slog.Attr{
				slog.String("target", "nats"),
				slog.Any("error", nats.ErrNoResponders),
			},
			wantDrop: true,
		},
		{
			name:  "drops wrapped nats no-responders sync error",
			level: slog.LevelError,
			msg:   "sync error",
			attrs: []slog.Attr{
				slog.String("target", "nats"),
				slog.Any("error", errors.New(
					"libfossil: sync: exchange round 0: nats: no responders available")),
			},
			wantDrop: true,
		},
		{
			name:  "passes non-nats sync error",
			level: slog.LevelError,
			msg:   "sync error",
			attrs: []slog.Attr{
				slog.String("target", "http"),
				slog.Any("error", nats.ErrNoResponders),
			},
			wantDrop:  false,
			shouldSee: "http",
		},
		{
			name:  "passes nats sync error with different cause",
			level: slog.LevelError,
			msg:   "sync error",
			attrs: []slog.Attr{
				slog.String("target", "nats"),
				slog.Any("error", errors.New("authentication failed")),
			},
			wantDrop:  false,
			shouldSee: "authentication failed",
		},
		{
			name:  "passes unrelated error",
			level: slog.LevelError,
			msg:   "agent stop error",
			attrs: []slog.Attr{
				slog.Any("error", errors.New("boom")),
			},
			wantDrop:  false,
			shouldSee: "agent stop error",
		},
		{
			name:  "passes info-level sync message",
			level: slog.LevelInfo,
			msg:   "sync details",
			attrs: []slog.Attr{
				slog.String("target", "nats"),
			},
			wantDrop:  false,
			shouldSee: "sync details",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
			h := suppressBenignSyncErrorHandler{inner: inner}

			r := slog.NewRecord(time.Time{}, tc.level, tc.msg, 0)
			r.AddAttrs(tc.attrs...)

			if err := h.Handle(context.Background(), r); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			out := buf.String()
			gotDrop := out == ""
			if gotDrop != tc.wantDrop {
				t.Fatalf("drop: got %v want %v (output=%q)", gotDrop, tc.wantDrop, out)
			}
			if !tc.wantDrop && tc.shouldSee != "" && !strings.Contains(out, tc.shouldSee) {
				t.Errorf("expected %q in output, got %q", tc.shouldSee, out)
			}
		})
	}
}
