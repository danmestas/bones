package tasks_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/testutil/natstest"
)

// openEventLogManager spins up a JetStream fixture and returns a
// Manager opened with EnableEventLog=true. Mirrors openTestManager
// from tasks_test.go but turns on the event-log stream so Tx is
// usable.
func openEventLogManager(
	t *testing.T,
) (*tasks.Manager, *nats.Conn, func()) {
	t.Helper()
	srv, cleanup := natstest.NewJetStreamServer(t)
	nc, err := nats.Connect(srv.ConnectedUrl())
	if err != nil {
		cleanup()
		t.Fatalf("nats.Connect: %v", err)
	}
	m, err := tasks.Open(context.Background(), nc, tasks.Config{
		BucketName:     "tasks-tx-test",
		HistoryDepth:   8,
		MaxValueSize:   8 * 1024,
		EnableEventLog: true,
	})
	if err != nil {
		nc.Close()
		cleanup()
		t.Fatalf("tasks.Open: %v", err)
	}
	return m, nc, func() {
		_ = m.Close()
		nc.Close()
		cleanup()
	}
}

// txNewTask returns a well-formed open task ready for tx.Create.
func txNewTask(id string) tasks.Task {
	now := time.Now().UTC()
	return tasks.Task{
		ID:            id,
		Title:         "tx example",
		Status:        tasks.StatusOpen,
		Files:         []string{"/work/x.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: tasks.SchemaVersion,
	}
}

// TestTx_NoMutationsReturnsError verifies that a Tx callback that
// makes no tx.X calls returns ErrNoMutations.
func TestTx_NoMutationsReturnsError(t *testing.T) {
	m, _, cleanup := openEventLogManager(t)
	defer cleanup()
	err := m.Tx(context.Background(), "missing-id", func(tx *tasks.Tx) error {
		return nil
	})
	if !errors.Is(err, tasks.ErrNoMutations) {
		t.Fatalf("want ErrNoMutations, got %v", err)
	}
}

// TestTx_OnlyMutationPath asserts via reflection that *Manager has no
// public mutation method other than Tx. This is the structural
// backstop for ADR 0052: any new mutation must be added under Tx, not
// as a parallel Manager method.
func TestTx_OnlyMutationPath(t *testing.T) {
	allowedMutationMethods := map[string]bool{
		"Tx": true,
	}
	// Methods on Manager that are NOT mutations are listed here so
	// future readers know the test's classification, not just its
	// contents.
	knownReadOrLifecycleMethods := map[string]bool{
		"Close":     true,
		"Get":       true,
		"KVForTest": true, // exposed for in-package test seeding only
		"List":      true,
		"Live":      true,
		"Purge":     true,
		"Recent":    true,
		"Replay":    true,
		"Watch":     true,
	}
	mgrType := reflect.TypeOf(&tasks.Manager{})
	for i := 0; i < mgrType.NumMethod(); i++ {
		name := mgrType.Method(i).Name
		switch {
		case allowedMutationMethods[name]:
			continue
		case knownReadOrLifecycleMethods[name]:
			continue
		default:
			// Heuristic: any unknown method on Manager is a regression.
			// New methods must be classified above explicitly.
			t.Errorf(
				"Manager has unclassified method %q — "+
					"classify in this test (mutation must go through Tx per ADR 0052)",
				name,
			)
		}
	}
}

// TestTx_CreateRoundtripsEvent verifies that tx.Create publishes a
// created event AND writes the KV record, and the event payload
// roundtrips cleanly.
func TestTx_CreateRoundtripsEvent(t *testing.T) {
	m, _, cleanup := openEventLogManager(t)
	defer cleanup()
	ctx := context.Background()
	rec := txNewTask("bones-roundtrip-1")
	err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
		return tx.Create(rec)
	})
	if err != nil {
		t.Fatalf("Tx.Create: %v", err)
	}
	// KV side
	got, _, err := m.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != rec.ID || got.Title != rec.Title {
		t.Fatalf("KV mismatch: got %+v", got)
	}
	if got.LastEventSeq == 0 {
		t.Fatalf("LastEventSeq should be non-zero after Tx.Create")
	}
	// Log side
	envs, err := m.Replay(ctx, tasks.LogReadOpts{FilterTaskID: rec.ID})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("want 1 event, got %d", len(envs))
	}
	if envs[0].Type != tasks.EventTypeCreated {
		t.Fatalf("want EventTypeCreated, got %s", envs[0].Type)
	}
	payload, err := tasks.DecodePayload(envs[0])
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	cp, ok := payload.(tasks.CreatedPayload)
	if !ok {
		t.Fatalf("payload type %T", payload)
	}
	if cp.Title != rec.Title {
		t.Fatalf("payload title mismatch: %q vs %q", cp.Title, rec.Title)
	}
}

// TestTx_UpdatePayloadCarriesOldNew verifies that tx.Update emits an
// event whose payload carries (field, old, new) tuples.
func TestTx_UpdatePayloadCarriesOldNew(t *testing.T) {
	m, _, cleanup := openEventLogManager(t)
	defer cleanup()
	ctx := context.Background()
	rec := txNewTask("bones-oldnew-1")
	if err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
		return tx.Create(rec)
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	change := tasks.MustFieldChange("title", rec.Title, "renamed")
	err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
		return tx.Update(func(t tasks.Task) (tasks.Task, error) {
			t.Title = "renamed"
			return t, nil
		}, change)
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	envs, _ := m.Replay(ctx, tasks.LogReadOpts{FilterTaskID: rec.ID})
	if len(envs) < 2 {
		t.Fatalf("want >=2 events, got %d", len(envs))
	}
	updateEnv := envs[1]
	if updateEnv.Type != tasks.EventTypeUpdated {
		t.Fatalf("want EventTypeUpdated, got %s", updateEnv.Type)
	}
	payload, err := tasks.DecodePayload(updateEnv)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	up := payload.(tasks.UpdatedPayload)
	if len(up.Changes) != 1 || up.Changes[0].Field != "title" {
		t.Fatalf("expected one change with field=title, got %+v", up.Changes)
	}
	var oldStr, newStr string
	if err := json.Unmarshal(up.Changes[0].Old, &oldStr); err != nil {
		t.Fatalf("old unmarshal: %v", err)
	}
	if err := json.Unmarshal(up.Changes[0].New, &newStr); err != nil {
		t.Fatalf("new unmarshal: %v", err)
	}
	if oldStr != rec.Title {
		t.Fatalf("want old=%q, got %q", rec.Title, oldStr)
	}
	if newStr != "renamed" {
		t.Fatalf("want new=renamed, got %q", newStr)
	}
}

// TestEventEnvelope_Roundtrip exercises every payload type's marshal
// and unmarshal path so cross-version readers see the same shape this
// version writes.
func TestEventEnvelope_Roundtrip(t *testing.T) {
	cases := []struct {
		name string
		typ  tasks.EventType
		p    any
	}{
		{
			"created", tasks.EventTypeCreated,
			tasks.CreatedPayload{Title: "x", Files: []string{"/a"}},
		},
		{
			"claimed", tasks.EventTypeClaimed,
			tasks.ClaimedPayload{AgentID: "a1", Slot: "s1", ClaimEpoch: 5},
		},
		{
			"unclaimed", tasks.EventTypeUnclaimed,
			tasks.UnclaimedPayload{Reason: "deadline"},
		},
		{
			"updated", tasks.EventTypeUpdated,
			tasks.UpdatedPayload{
				Changes: []tasks.FieldChange{
					tasks.MustFieldChange("k", 1, 2),
				},
			},
		},
		{
			"linked", tasks.EventTypeLinked,
			tasks.LinkedPayload{OtherID: "bones-other", EdgeType: "blocks"},
		},
		{
			"slot_changed", tasks.EventTypeSlotChanged,
			tasks.SlotChangedPayload{From: "a", To: "b"},
		},
		{
			"closed", tasks.EventTypeClosed,
			tasks.ClosedPayload{AgentID: "a1", Reason: "done"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, err := tasks.EncodeEnvelope(c.typ, "bones-x", c.p)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			raw, err := tasks.MarshalEnvelope(env)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := tasks.UnmarshalEnvelope(raw)
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Type != c.typ {
				t.Fatalf("type roundtrip: want %s, got %s", c.typ, got.Type)
			}
			if got.TaskID != "bones-x" {
				t.Fatalf("task_id roundtrip: %s", got.TaskID)
			}
			if _, err := tasks.DecodePayload(got); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
		})
	}
}

// TestTaskTally_Counts verifies that the shared TaskTally function
// derives the expected counts from a sequence of events. Both `bones
// status` and `bones tasks status` consume this function; if it
// drifts, both surfaces drift together.
func TestTaskTally_Counts(t *testing.T) {
	mk := func(typ tasks.EventType, id string) tasks.EventEnvelope {
		env, _ := tasks.EncodeEnvelope(typ, id, struct{}{})
		return env
	}
	envs := []tasks.EventEnvelope{
		mk(tasks.EventTypeCreated, "a"),
		mk(tasks.EventTypeCreated, "b"),
		mk(tasks.EventTypeCreated, "c"),
		mk(tasks.EventTypeClaimed, "b"),
		mk(tasks.EventTypeClosed, "c"),
	}
	got := tasks.TaskTally(envs)
	want := tasks.Tally{Open: 1, Claimed: 1, Closed: 1, Total: 3}
	if got != want {
		t.Fatalf("want %+v, got %+v", want, got)
	}
}

// TestMigration_Idempotent verifies that running Migrate twice on the
// same KV state does not produce duplicate synthetic events.
func TestMigration_Idempotent(t *testing.T) {
	srv, cleanup := natstest.NewJetStreamServer(t)
	defer cleanup()
	nc, err := nats.Connect(srv.ConnectedUrl())
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	cfg := tasks.Config{
		BucketName:     "tasks-mig-test",
		HistoryDepth:   8,
		MaxValueSize:   8 * 1024,
		EnableEventLog: true,
	}
	// Seed the bucket via AdminWrite *before* opening with event log.
	// First open without event log so Migrate doesn't fire on Open.
	bootCfg := cfg
	bootCfg.EnableEventLog = false
	boot, err := tasks.Open(context.Background(), nc, bootCfg)
	if err != nil {
		t.Fatalf("boot Open: %v", err)
	}
	aw := tasks.NewAdminWrite(boot)
	for _, id := range []string{"bones-mig-a", "bones-mig-b"} {
		if err := aw.Create(context.Background(), txNewTask(id)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	_ = boot.Close()

	// Now open with event log — Migrate runs as part of Open.
	m, err := tasks.Open(context.Background(), nc, cfg)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	envs1, err := m.Replay(context.Background(), tasks.LogReadOpts{})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(envs1) < 2 {
		t.Fatalf("want >=2 synthetic events for 2 seeded tasks, got %d", len(envs1))
	}
	_ = m.Close()

	// Re-open: marker is set, Migrate is a no-op.
	m2, err := tasks.Open(context.Background(), nc, cfg)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = m2.Close() }()
	envs2, err := m2.Replay(context.Background(), tasks.LogReadOpts{})
	if err != nil {
		t.Fatalf("replay 2: %v", err)
	}
	if len(envs2) != len(envs1) {
		t.Fatalf("re-Migrate emitted duplicates: first=%d second=%d", len(envs1), len(envs2))
	}
}

// TestReplayCorrectness verifies that a sequence of Tx-driven mutations
// produces a log whose Tally matches the live KV projection.
func TestReplayCorrectness(t *testing.T) {
	m, _, cleanup := openEventLogManager(t)
	defer cleanup()
	ctx := context.Background()
	for i, op := range []struct {
		op string
		id string
	}{
		{"create", "bones-r-a"},
		{"create", "bones-r-b"},
		{"create", "bones-r-c"},
		{"claim", "bones-r-b"},
		{"close", "bones-r-c"},
	} {
		switch op.op {
		case "create":
			rec := txNewTask(op.id)
			if err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
				return tx.Create(rec)
			}); err != nil {
				t.Fatalf("step %d create %s: %v", i, op.id, err)
			}
		case "claim":
			if err := m.Tx(ctx, op.id, func(tx *tasks.Tx) error {
				return tx.Claim(tasks.ClaimArgs{
					AgentID:    "agent-x",
					ClaimEpoch: 1,
					Mutate: func(t tasks.Task) (tasks.Task, error) {
						t.Status = tasks.StatusClaimed
						t.ClaimedBy = "agent-x"
						t.ClaimEpoch = 1
						return t, nil
					},
				})
			}); err != nil {
				t.Fatalf("step %d claim %s: %v", i, op.id, err)
			}
		case "close":
			mutate := func(t tasks.Task) (tasks.Task, error) {
				now := time.Now().UTC()
				t.Status = tasks.StatusClosed
				t.ClosedAt = &now
				t.ClaimedBy = ""
				t.UpdatedAt = now
				return t, nil
			}
			if err := m.Tx(ctx, op.id, func(tx *tasks.Tx) error {
				return tx.Close("agent-x", "done", mutate)
			}); err != nil {
				t.Fatalf("step %d close %s: %v", i, op.id, err)
			}
		}
	}
	envs, err := m.Replay(ctx, tasks.LogReadOpts{})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	tally := tasks.TaskTally(envs)
	if tally.Open != 1 || tally.Claimed != 1 || tally.Closed != 1 || tally.Total != 3 {
		t.Fatalf("want {1,1,1,3} got %+v (envs=%d)", tally, len(envs))
	}
}

// TestReplay_FromAndSinceMutuallyExclusive verifies the watch-flag
// validation per ADR 0052.
func TestReplay_FromAndSinceMutuallyExclusive(t *testing.T) {
	m, _, cleanup := openEventLogManager(t)
	defer cleanup()
	_, err := m.Replay(context.Background(), tasks.LogReadOpts{
		FromSeq: 1,
		Since:   time.Hour,
	})
	if err == nil {
		t.Fatalf("want error for both --from and --since set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want 'mutually exclusive' in error, got %q", err.Error())
	}
}
