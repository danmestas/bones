package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
)

// The CAS tests live alongside tasks_test.go. They exercise the
// revision-gated Update path: the retry hook proves the loop re-runs
// on conflict, and a high-concurrency stress run proves convergence
// under arbitrary scheduling.

// TestUpdate_CAS_RetryFires forces exactly one revision conflict by
// arming the pre-write hook to Put-once before Update's CAS write.
// After the hook fires once, it disarms itself — so attempt 1 loses,
// retry fires, attempt 2 wins. Asserts both the retry counter
// incremented (the retry path actually ran) and the final record is
// the Update's intended mutation, not the hook's Put payload.
func TestUpdate_CAS_RetryFires(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-retry001"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var retries atomic.Int32
	restoreRetry := tasks.SetCASRetryHookForTest(func() {
		retries.Add(1)
	})
	defer restoreRetry()

	kv := m.KVForTest()
	var armed atomic.Bool
	armed.Store(true)
	restorePre := tasks.SetUpdatePreWriteHookForTest(func() {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		competing := newTask(id)
		competing.Title = "competing write"
		competing.UpdatedAt = time.Now().UTC()
		payload, err := tasks.EncodeForTest(competing)
		if err != nil {
			return
		}
		_, _ = kv.Put(ctx, id, payload)
	})
	defer restorePre()

	err := m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.Status = tasks.StatusClaimed
		t.ClaimedBy = "agent-a"
		t.UpdatedAt = time.Now().UTC()
		return t, nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := retries.Load(); got < 1 {
		t.Fatalf("retry hook fired %d times, want >= 1", got)
	}

	got, _, err := m.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != tasks.StatusClaimed || got.ClaimedBy != "agent-a" {
		t.Fatalf(
			"post-CAS state: status=%q claimed_by=%q, want claimed/agent-a",
			got.Status, got.ClaimedBy,
		)
	}
}

// TestUpdate_CAS_ConcurrentContention is the stress variant that proves
// the CAS path end-to-end. N goroutines race to mutate the same record
// (each appending a unique key into the Context map); every goroutine
// must eventually succeed, and the final record's Context must contain
// every racer's key. If the CAS loop is broken, at least one Update
// would either lose silently or surface ErrCASConflict.
func TestUpdate_CAS_ConcurrentContention(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-stress01"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Keep racers well below maxCASRetries (8) so no single racer can
	// lose all eight rounds; stress the loop without staging a forced-
	// exhaustion (that path has its own test below).
	const racers = 4
	startGun := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, racers)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		key := fmt.Sprintf("k%02d", i)
		go func(idx int, k string) {
			defer wg.Done()
			<-startGun
			errs[idx] = m.Update(
				ctx, id,
				func(t tasks.Task) (tasks.Task, error) {
					if t.Context == nil {
						t.Context = map[string]string{}
					}
					t.Context[k] = "v"
					t.UpdatedAt = time.Now().UTC()
					return t, nil
				},
			)
		}(i, key)
	}
	close(startGun)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}

	got, _, err := m.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for i := 0; i < racers; i++ {
		key := fmt.Sprintf("k%02d", i)
		if _, ok := got.Context[key]; !ok {
			t.Fatalf(
				"Context missing key %q after %d racers: %+v",
				key, racers, got.Context,
			)
		}
	}
}

// TestUpdate_CAS_ExhaustedRetries verifies that the retry bound is
// real. We simulate a perpetual-conflict scenario by installing a
// pre-write hook that advances the revision before every Update call;
// every attempt fails because the revision the CAS loop holds is
// always stale, so after maxCASRetries iterations the Update must
// surface ErrCASConflict.
func TestUpdate_CAS_ExhaustedRetries(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-exhaust0"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	kv := m.KVForTest()
	restore := tasks.SetUpdatePreWriteHookForTest(func() {
		doomed := newTask(id)
		doomed.Title = fmt.Sprintf("churn %d", time.Now().UnixNano())
		payload, _ := tasks.EncodeForTest(doomed)
		_, _ = kv.Put(ctx, id, payload)
	})
	defer restore()

	// The mutate closure always succeeds; the CAS race is what fails.
	err := m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.Title = "target"
		return t, nil
	})
	if !errors.Is(err, tasks.ErrCASConflict) {
		t.Fatalf(
			"Update under perpetual conflict: got %v, want ErrCASConflict",
			err,
		)
	}
}
