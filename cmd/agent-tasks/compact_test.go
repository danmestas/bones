package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunCompactCadence_ZeroRunsOnce(t *testing.T) {
	calls := 0
	err := runCompactCadence(context.Background(), 0, nil, func(context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("runCompactCadence: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}

func TestRunCompactCadence_RepeatsUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 3)
	calls := 0
	go func() {
		ticks <- time.Now()
		ticks <- time.Now()
	}()
	want := errors.New("stop")
	err := runCompactCadence(ctx, time.Second, ticks, func(context.Context) error {
		calls++
		if calls == 2 {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}
