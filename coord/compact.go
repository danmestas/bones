package coord

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/danmestas/libfossil"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/tasks"
)

// Summarizer compresses an eligible closed task into a textual summary
// that becomes the body of the compaction artifact. The Summarizer is
// caller-supplied so the compaction policy stays orthogonal to the
// commit substrate.
type Summarizer interface {
	Summarize(context.Context, CompactInput) (string, error)
}

// CompactOptions parameterizes a Compact run.
type CompactOptions struct {
	MinAge     time.Duration
	Limit      int
	Now        func() time.Time
	Summarizer Summarizer
	Prune      bool
}

// CompactInput is the read-only view of a task that the Summarizer sees.
type CompactInput struct {
	TaskID       TaskID
	Title        string
	Files        []string
	Context      map[string]string
	CreatedAt    time.Time
	ClosedAt     time.Time
	ClosedBy     string
	ClosedReason string
	CompactLevel uint8
}

// CompactedTask is the per-task output of Compact.
type CompactedTask struct {
	TaskID       TaskID
	Path         string
	Rev          RevID
	CompactLevel uint8
	Pruned       bool
}

// CompactResult aggregates the per-task results from a single Compact
// invocation.
type CompactResult struct {
	Tasks []CompactedTask
}

// Compact summarizes eligible closed tasks and writes one artifact per
// task into the leaf's libfossil repo as a new checkin authored by the
// slot. Eligibility: status=closed, CompactLevel=0, ClosedAt older
// than opts.MinAge. The summary body is produced by opts.Summarizer
// and the artifact path is `compaction/tasks/<TaskID>/level-<N>.md`.
//
// All writes route through l.agent.Repo() — the only *libfossil.Repo
// handle to leaf.fossil in this process. After each commit, SyncNow
// triggers a sync round so the artifact propagates to the hub.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), receiver non-nil, opts.Limit > 0, opts.Summarizer
// non-nil. Operator errors from substrate reads (tasks.List) and the
// per-task summarize/commit/update/archive paths surface wrapped.
func (l *Leaf) Compact(ctx context.Context, opts CompactOptions) (CompactResult, error) {
	assert.NotNil(l, "coord.Leaf.Compact: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.Compact: ctx is nil")
	assert.Precondition(opts.Limit > 0, "coord.Leaf.Compact: Limit must be > 0")
	assert.NotNil(opts.Summarizer, "coord.Leaf.Compact: Summarizer is nil")
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	records, err := l.coord.listTasks(ctx)
	if err != nil {
		return CompactResult{}, fmt.Errorf("coord.Leaf.Compact: %w", err)
	}
	eligible := eligibleCompactionTasks(records, nowFn(), opts.MinAge)
	if len(eligible) > opts.Limit {
		eligible = eligible[:opts.Limit]
	}
	result := CompactResult{Tasks: make([]CompactedTask, 0, len(eligible))}
	for _, rec := range eligible {
		compacted, err := l.compactOne(
			ctx, rec, nowFn(), opts.Summarizer, opts.Prune,
		)
		if err != nil {
			return result, err
		}
		result.Tasks = append(result.Tasks, compacted)
	}
	return result, nil
}

// eligibleCompactionTasks selects records eligible for compaction:
// closed tasks at level 0 whose ClosedAt is older than minAge. Sorted
// by ClosedAt ascending so the oldest-first behavior is deterministic.
func eligibleCompactionTasks(
	records []tasks.Task,
	now time.Time,
	minAge time.Duration,
) []tasks.Task {
	out := make([]tasks.Task, 0, len(records))
	for _, rec := range records {
		if rec.Status != tasks.StatusClosed || rec.ClosedAt == nil {
			continue
		}
		if rec.CompactLevel != 0 {
			continue
		}
		if now.Sub(*rec.ClosedAt) < minAge {
			continue
		}
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ClosedAt.Before(*out[j].ClosedAt)
	})
	return out
}

// compactOne runs the summarize → commit → update → (optional) archive
// pipeline for a single record. The commit goes through l.agent.Repo()
// so the artifact lives on the same fossil handle as the leaf's
// regular commits, and SyncNow propagates it to the hub.
func (l *Leaf) compactOne(
	ctx context.Context,
	rec tasks.Task,
	now time.Time,
	summarizer Summarizer,
	prune bool,
) (CompactedTask, error) {
	input := compactInput(rec)
	summary, err := summarizer.Summarize(ctx, input)
	if err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Leaf.Compact: summarize %s: %w", rec.ID, err)
	}
	level := rec.CompactLevel + 1
	path := compactArtifactPath(TaskID(rec.ID), level)
	body := compactArtifactBody(input, summary)
	repo := l.agent.Repo()
	_, uuid, err := repo.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: normalizeLeadingSlash(path), Content: []byte(body)},
		},
		Comment: compactCommitMessage(rec.ID, level),
		User:    l.slotID,
	})
	if err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Leaf.Compact: commit %s: %w", rec.ID, err)
	}
	l.agent.SyncNow()
	originalSize, err := compactOriginalSize(rec)
	if err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Leaf.Compact: original size %s: %w", rec.ID, err)
	}
	if err := l.coord.updateTask(ctx, rec.ID, func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusClosed {
			return cur, ErrTaskAlreadyClosed
		}
		cur.OriginalSize = originalSize
		cur.CompactLevel = level
		cur.CompactedAt = &now
		cur.UpdatedAt = now
		return cur, nil
	}); err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Leaf.Compact: update %s: %w", rec.ID, err)
	}
	if prune {
		if err := l.archiveAndPurgeCompactedTask(ctx, rec.ID); err != nil {
			return CompactedTask{}, err
		}
	}
	return CompactedTask{
		TaskID:       TaskID(rec.ID),
		Path:         path,
		Rev:          RevID(uuid),
		CompactLevel: level,
		Pruned:       prune,
	}, nil
}

// archiveAndPurgeCompactedTask copies the compacted record into the
// archive bucket then purges it from the hot tasks bucket. Idempotent
// against repeat compactions: a record already in the archive bucket
// is left in place.
func (l *Leaf) archiveAndPurgeCompactedTask(
	ctx context.Context, id string,
) error {
	if err := l.coord.getAndArchiveTask(ctx, id); err != nil {
		return fmt.Errorf("coord.Leaf.Compact: archive/purge %s: %w", id, err)
	}
	return nil
}

// compactInput projects a tasks.Task into the read-only CompactInput
// the Summarizer sees. Maps and slices are copied so the Summarizer
// cannot mutate the substrate's view.
func compactInput(rec tasks.Task) CompactInput {
	ctxCopy := map[string]string{}
	for k, v := range rec.Context {
		ctxCopy[k] = v
	}
	files := make([]string, len(rec.Files))
	copy(files, rec.Files)
	closedAt := time.Time{}
	if rec.ClosedAt != nil {
		closedAt = *rec.ClosedAt
	}
	return CompactInput{
		TaskID:       TaskID(rec.ID),
		Title:        rec.Title,
		Files:        files,
		Context:      ctxCopy,
		CreatedAt:    rec.CreatedAt,
		ClosedAt:     closedAt,
		ClosedBy:     rec.ClosedBy,
		ClosedReason: rec.ClosedReason,
		CompactLevel: rec.CompactLevel,
	}
}

// compactArtifactPath builds the canonical path inside the fossil
// repo where the level-N artifact lives.
func compactArtifactPath(taskID TaskID, level uint8) string {
	return fmt.Sprintf("compaction/tasks/%s/level-%d.md", taskID, level)
}

// compactCommitMessage builds the human-readable commit message for
// the artifact-bearing checkin.
func compactCommitMessage(taskID string, level uint8) string {
	return fmt.Sprintf("compact task %s level %d", taskID, level)
}

// compactArtifactBody renders the summary into the markdown body that
// is the actual file content in the fossil checkin.
func compactArtifactBody(in CompactInput, summary string) string {
	return fmt.Sprintf(
		"# Compaction for %s\n\nlevel: %d\n\nsummary:\n%s\n",
		in.TaskID,
		in.CompactLevel+1,
		summary,
	)
}

// compactOriginalSize returns the JSON-serialized byte length of rec —
// a stable proxy for "how much room the task takes up in the hot
// bucket" used by the OriginalSize metadata field.
func compactOriginalSize(rec tasks.Task) (uint64, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, err
	}
	return uint64(len(data)), nil
}
