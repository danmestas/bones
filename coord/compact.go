package coord

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/danmestas/agent-infra/internal/assert"
	ifossil "github.com/danmestas/agent-infra/internal/fossil"
	"github.com/danmestas/agent-infra/internal/tasks"
)

type Summarizer interface {
	Summarize(context.Context, CompactInput) (string, error)
}

type CompactOptions struct {
	MinAge     time.Duration
	Limit      int
	Now        func() time.Time
	Summarizer Summarizer
}

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

type CompactedTask struct {
	TaskID       TaskID
	Path         string
	Rev          RevID
	CompactLevel uint8
}

type CompactResult struct {
	Tasks []CompactedTask
}

func (c *Coord) Compact(
	ctx context.Context,
	opts CompactOptions,
) (CompactResult, error) {
	c.assertOpen("Compact")
	assert.NotNil(ctx, "coord.Compact: ctx is nil")
	assert.Precondition(opts.Limit > 0, "coord.Compact: Limit must be > 0")
	assert.NotNil(opts.Summarizer, "coord.Compact: Summarizer is nil")
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	records, err := c.sub.tasks.List(ctx)
	if err != nil {
		return CompactResult{}, fmt.Errorf("coord.Compact: %w", err)
	}
	eligible := eligibleCompactionTasks(records, nowFn(), opts.MinAge)
	if len(eligible) > opts.Limit {
		eligible = eligible[:opts.Limit]
	}
	result := CompactResult{Tasks: make([]CompactedTask, 0, len(eligible))}
	for _, rec := range eligible {
		compacted, err := c.compactOne(ctx, rec, nowFn(), opts.Summarizer)
		if err != nil {
			return result, err
		}
		result.Tasks = append(result.Tasks, compacted)
	}
	return result, nil
}

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

func (c *Coord) compactOne(
	ctx context.Context,
	rec tasks.Task,
	now time.Time,
	summarizer Summarizer,
) (CompactedTask, error) {
	input := compactInput(rec)
	summary, err := summarizer.Summarize(ctx, input)
	if err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Compact: summarize %s: %w", rec.ID, err)
	}
	level := rec.CompactLevel + 1
	path := compactArtifactPath(TaskID(rec.ID), level)
	body := compactArtifactBody(input, summary)
	rev, err := c.sub.fossil.Commit(ctx, compactCommitMessage(rec.ID, level), []ifossil.File{{
		Path: path, Content: []byte(body),
	}}, "")
	if err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Compact: commit %s: %w", rec.ID, err)
	}
	originalSize, err := compactOriginalSize(rec)
	if err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Compact: original size %s: %w", rec.ID, err)
	}
	err = c.sub.tasks.Update(ctx, rec.ID, func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusClosed {
			return cur, ErrTaskAlreadyClosed
		}
		cur.OriginalSize = originalSize
		cur.CompactLevel = level
		cur.CompactedAt = &now
		cur.UpdatedAt = now
		return cur, nil
	})
	if err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Compact: update %s: %w", rec.ID, err)
	}
	return CompactedTask{
		TaskID:       TaskID(rec.ID),
		Path:         path,
		Rev:          RevID(rev),
		CompactLevel: level,
	}, nil
}

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

func compactArtifactPath(taskID TaskID, level uint8) string {
	return fmt.Sprintf("compaction/tasks/%s/level-%d.md", taskID, level)
}

func compactCommitMessage(taskID string, level uint8) string {
	return fmt.Sprintf("compact task %s level %d", taskID, level)
}

func compactArtifactBody(in CompactInput, summary string) string {
	return fmt.Sprintf(
		"# Compaction for %s\n\nlevel: %d\n\nsummary:\n%s\n",
		in.TaskID,
		in.CompactLevel+1,
		summary,
	)
}

func compactOriginalSize(rec tasks.Task) (uint64, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, err
	}
	return uint64(len(data)), nil
}
