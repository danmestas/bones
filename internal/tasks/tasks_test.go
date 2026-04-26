package tasks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/agent-infra/internal/tasks"
	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// openTestManager spins up a JetStream-enabled fixture and returns a
// fresh Manager bound to a bucket unique to each test. Tests should
// defer the returned cleanup to free the embedded server and store dir.
func openTestManager(
	t *testing.T,
) (*tasks.Manager, *nats.Conn, func()) {
	t.Helper()
	srv, cleanup := natstest.NewJetStreamServer(t)
	nc, err := nats.Connect(srv.ConnectedUrl())
	if err != nil {
		cleanup()
		t.Fatalf("nats.Connect: %v", err)
	}
	cfg := tasks.Config{
		BucketName:   "tasks-test",
		HistoryDepth: 8,
		MaxValueSize: 8 * 1024,
	}
	m, err := tasks.Open(context.Background(), nc, cfg)
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

// newTask returns a well-formed open task with the given ID. Tests
// override fields after construction to exercise specific invariant
// violations; the baseline is chosen so it passes Create as-is.
func newTask(id string) tasks.Task {
	now := time.Now().UTC()
	return tasks.Task{
		ID:            id,
		Title:         "example task",
		Status:        tasks.StatusOpen,
		Files:         []string{"/work/a.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: tasks.SchemaVersion,
	}
}

// requirePanic verifies that fn panics with a message that contains
// want. Mirrors the same helper in internal/holds/holds_test.go.
func requirePanic(t *testing.T, fn func(), want string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", want)
		}
		if !strings.Contains(fmt.Sprint(r), want) {
			t.Fatalf("panic %q does not contain %q", r, want)
		}
	}()
	fn()
}

func TestOpenClose_Lifecycle(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()

	if err := m.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Close is idempotent.
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Methods after Close return ErrClosed.
	_, _, err := m.Get(context.Background(), "some-id")
	if !errors.Is(err, tasks.ErrClosed) {
		t.Fatalf("Get after Close: got %v, want ErrClosed", err)
	}
}

func TestCreate_HappyPath(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-aaaaaaaa"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, rev, err := m.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != id {
		t.Fatalf("ID: got %q, want %q", got.ID, id)
	}
	if got.Status != tasks.StatusOpen {
		t.Fatalf("Status: got %q, want open", got.Status)
	}
	if rev == 0 {
		t.Fatalf("revision: got 0, want > 0")
	}
}

func TestCreate_Duplicate_ReturnsAlreadyExists(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-aaaaaaab"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	err := m.Create(ctx, newTask(id))
	if !errors.Is(err, tasks.ErrAlreadyExists) {
		t.Fatalf("Create 2: got %v, want ErrAlreadyExists", err)
	}
}

func TestGet_Absent_ReturnsNotFound(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()

	_, _, err := m.Get(context.Background(), "agent-infra-ghost0000")
	if !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("Get absent: got %v, want ErrNotFound", err)
	}
}

func TestUpdate_HappyPath(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-update001"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, preRev, _ := m.Get(ctx, id)

	err := m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.Status = tasks.StatusClaimed
		t.ClaimedBy = "agent-a"
		t.UpdatedAt = time.Now().UTC()
		return t, nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, postRev, err := m.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != tasks.StatusClaimed {
		t.Fatalf("Status: got %q, want claimed", got.Status)
	}
	if got.ClaimedBy != "agent-a" {
		t.Fatalf("ClaimedBy: got %q, want agent-a", got.ClaimedBy)
	}
	if postRev <= preRev {
		t.Fatalf(
			"revision did not advance: pre=%d post=%d",
			preRev, postRev,
		)
	}
}

func TestUpdate_Invariant11_Violation_Rejected(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-inv110001"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err := m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.Status = tasks.StatusClaimed
		// ClaimedBy deliberately empty — the invariant-11 violation.
		return t, nil
	})
	if !errors.Is(err, tasks.ErrInvariant11) {
		t.Fatalf("Update: got %v, want ErrInvariant11", err)
	}

	// Bucket state must remain the pre-mutation record.
	got, _, err := m.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != tasks.StatusOpen {
		t.Fatalf("Status: got %q, want open", got.Status)
	}
}

func TestUpdate_InvalidTransition_Rejected(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-dag00001"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Walk open → closed (a legal edge) so we can then attempt a
	// backwards closed → open edge.
	now := time.Now().UTC()
	err := m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.Status = tasks.StatusClosed
		t.ClosedAt = &now
		t.ClosedBy = "agent-a"
		t.ClosedReason = "obsolete"
		return t, nil
	})
	if err != nil {
		t.Fatalf("Update to closed: %v", err)
	}

	err = m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.Status = tasks.StatusOpen
		t.ClosedAt = nil
		t.ClosedBy = ""
		t.ClosedReason = ""
		return t, nil
	})
	if !errors.Is(err, tasks.ErrInvalidTransition) {
		t.Fatalf("Update: got %v, want ErrInvalidTransition", err)
	}
}

func TestUpdate_ClosedIsTerminal_RejectsSelfEdge(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-closed01"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := time.Now().UTC()
	err := m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.Status = tasks.StatusClosed
		t.ClosedAt = &now
		t.ClosedBy = "agent-a"
		t.ClosedReason = "done"
		return t, nil
	})
	if err != nil {
		t.Fatalf("Update to closed: %v", err)
	}
	// closed→closed (metadata-only update on a terminal record) must
	// fail so the closed snapshot stays immutable.
	err = m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.ClosedReason = "mutated after close"
		return t, nil
	})
	if !errors.Is(err, tasks.ErrInvalidTransition) {
		t.Fatalf(
			"closed→closed: got %v, want ErrInvalidTransition", err,
		)
	}
}

func TestUpdate_ClosedCompactionMetadata_AllowsSelfEdge(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-closed02"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := time.Now().UTC()
	err := m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.Status = tasks.StatusClosed
		t.ClosedAt = &now
		t.ClosedBy = "agent-a"
		t.ClosedReason = "done"
		return t, nil
	})
	if err != nil {
		t.Fatalf("Update to closed: %v", err)
	}
	compactedAt := now.Add(time.Hour)
	err = m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.OriginalSize = 123
		t.CompactLevel = 1
		t.CompactedAt = &compactedAt
		t.UpdatedAt = compactedAt
		return t, nil
	})
	if err != nil {
		t.Fatalf("Update compaction metadata: %v", err)
	}
	got, _, err := m.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OriginalSize != 123 || got.CompactLevel != 1 {
		t.Fatalf("got metadata=%+v", got)
	}
	if got.CompactedAt == nil || !got.CompactedAt.Equal(compactedAt) {
		t.Fatalf("CompactedAt=%v, want %v", got.CompactedAt, compactedAt)
	}
}

func TestUpdate_ValueTooLarge_Rejected(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-big00001"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	bigCtx := make(map[string]string, 1)
	bigCtx["blob"] = strings.Repeat("x", 16*1024)

	err := m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.Context = bigCtx
		return t, nil
	})
	if !errors.Is(err, tasks.ErrValueTooLarge) {
		t.Fatalf("Update: got %v, want ErrValueTooLarge", err)
	}
}

func TestUpdate_MutateError_Propagates(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-mut00001"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sentinel := errors.New("caller sentinel")
	err := m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		return t, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Update: got %v, want sentinel", err)
	}
}

func TestList_ReturnsAllRecords(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()

	ids := []string{
		"agent-infra-list0001",
		"agent-infra-list0002",
		"agent-infra-list0003",
	}
	for _, id := range ids {
		if err := m.Create(ctx, newTask(id)); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	got, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("List: got %d records, want %d", len(got), len(ids))
	}
	seen := make(map[string]struct{}, len(got))
	for _, t := range got {
		seen[t.ID] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			t.Fatalf("List missing %q: %+v", id, got)
		}
	}
}

func TestList_EmptyBucket_ReturnsNil(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()

	got, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List empty: got %d records, want 0", len(got))
	}
}

func TestPurge_RemovesRecord(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-purge001"

	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Purge(ctx, id); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	_, _, err := m.Get(ctx, id)
	if !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("Get after Purge: got %v, want ErrNotFound", err)
	}
	got, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List after Purge: got %d records, want 0", len(got))
	}
}

func TestCreate_InvalidStatus_Rejected(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()

	bad := newTask("agent-infra-status01")
	bad.Status = "totally-bogus"
	err := m.Create(context.Background(), bad)
	if !errors.Is(err, tasks.ErrInvalidStatus) {
		t.Fatalf("Create: got %v, want ErrInvalidStatus", err)
	}
}

func TestCreate_Invariant11_OnCreate_Rejected(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()

	bad := newTask("agent-infra-inv1101c")
	bad.Status = tasks.StatusClaimed
	// ClaimedBy deliberately empty.
	err := m.Create(context.Background(), bad)
	if !errors.Is(err, tasks.ErrInvariant11) {
		t.Fatalf("Create: got %v, want ErrInvariant11", err)
	}
}

func TestInvariant_NilCtx(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	var ctx context.Context

	requirePanic(t, func() {
		_ = m.Create(ctx, newTask("agent-infra-ctx00001"))
	}, "ctx is nil")
	requirePanic(t, func() {
		_, _, _ = m.Get(ctx, "agent-infra-ctx00002")
	}, "ctx is nil")
	requirePanic(t, func() {
		_ = m.Update(ctx, "agent-infra-ctx00003",
			func(t tasks.Task) (tasks.Task, error) { return t, nil })
	}, "ctx is nil")
	requirePanic(t, func() {
		_, _ = m.List(ctx)
	}, "ctx is nil")
	requirePanic(t, func() {
		_, _ = m.Watch(ctx)
	}, "ctx is nil")
}

func TestInvariant_EmptyID(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()

	requirePanic(t, func() {
		_ = m.Create(ctx, newTask(""))
	}, "t.ID is empty")
	requirePanic(t, func() {
		_, _, _ = m.Get(ctx, "")
	}, "id is empty")
	requirePanic(t, func() {
		_ = m.Update(ctx, "",
			func(t tasks.Task) (tasks.Task, error) { return t, nil })
	}, "id is empty")
}

func TestInvariant_NilMutate(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()

	requirePanic(t, func() {
		_ = m.Update(context.Background(), "x", nil)
	}, "mutate is nil")
}

func TestTask_ClaimEpoch_DecodeMissing(t *testing.T) {
	// Legacy JSON (no claim_epoch field) must decode with ClaimEpoch=0.
	legacy := []byte(`{"id":"t1","title":"x","status":"open",` +
		`"files":["/a"],"created_at":"2026-01-01T00:00:00Z",` +
		`"updated_at":"2026-01-01T00:00:00Z","schema_version":1}`)
	var got tasks.Task
	if err := json.Unmarshal(legacy, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ClaimEpoch != 0 {
		t.Fatalf("want ClaimEpoch=0 on missing field, got %d", got.ClaimEpoch)
	}
}

func TestGet_MigratesLegacySchemaVersionOnRead(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	id := "agent-infra-legacy01"
	legacy := []byte(`{"id":"` + id + `","title":"legacy","status":"open",` +
		`"files":["/a"],"created_at":"2026-01-01T00:00:00Z",` +
		`"updated_at":"2026-01-01T00:00:00Z","schema_version":1}`)
	if _, err := m.KVForTest().Create(ctx, id, legacy); err != nil {
		t.Fatalf("raw Create: %v", err)
	}
	got, _, err := m.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SchemaVersion != tasks.SchemaVersion {
		t.Fatalf("SchemaVersion=%d, want %d", got.SchemaVersion, tasks.SchemaVersion)
	}
	reloaded, _, err := m.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get reloaded: %v", err)
	}
	if reloaded.SchemaVersion != tasks.SchemaVersion {
		t.Fatalf("reloaded SchemaVersion=%d, want %d", reloaded.SchemaVersion, tasks.SchemaVersion)
	}
}

func TestTask_ClaimEpoch_EncodeRoundTrip(t *testing.T) {
	in := tasks.Task{
		ID: "t1", Title: "x", Status: tasks.StatusClaimed,
		ClaimedBy: "A", Files: []string{"/a"},
		ClaimEpoch:    7,
		SchemaVersion: tasks.SchemaVersion,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := tasks.Task{}
	err = json.Unmarshal(b, &out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ClaimEpoch != 7 {
		t.Fatalf("want ClaimEpoch=7 round-tripped, got %d", out.ClaimEpoch)
	}
}

func TestInvariant_UseAfterClose(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := context.Background()
	if err := m.Create(ctx, newTask("x")); !errors.Is(
		err, tasks.ErrClosed,
	) {
		t.Fatalf("Create after Close: got %v, want ErrClosed", err)
	}
	if _, _, err := m.Get(ctx, "x"); !errors.Is(
		err, tasks.ErrClosed,
	) {
		t.Fatalf("Get after Close: got %v, want ErrClosed", err)
	}
	if err := m.Update(ctx, "x",
		func(t tasks.Task) (tasks.Task, error) { return t, nil },
	); !errors.Is(err, tasks.ErrClosed) {
		t.Fatalf("Update after Close: got %v, want ErrClosed", err)
	}
	if _, err := m.List(ctx); !errors.Is(err, tasks.ErrClosed) {
		t.Fatalf("List after Close: got %v, want ErrClosed", err)
	}
	if _, err := m.Watch(ctx); !errors.Is(err, tasks.ErrClosed) {
		t.Fatalf("Watch after Close: got %v, want ErrClosed", err)
	}
}
