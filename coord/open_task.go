package coord

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// taskIDProject is the fixed project prefix the TaskID generator emits
// per ADR 0005. Callers never supply it; it is baked into the coord
// package for agent-infra and would be a new-ADR concern to change.
const taskIDProject = "agent-infra"

// taskIDSuffixLen is the length of the random lowercase-alphanumeric
// suffix appended after the project prefix and dash. ADR 0005 fixes
// this at 8 for the ~41 bits of entropy discussed in that document.
const taskIDSuffixLen = 8

// taskIDAlphabet is the 36-symbol lowercase-alphanumeric alphabet the
// generator draws from. Kept as a string literal rather than a slice so
// Go can intern the bytes and the index-into-string path in
// generateTaskID avoids an extra allocation per character.
const taskIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// taskIDPattern is the compiled regexp that validates a generated
// TaskID's shape before Create. Compiled once at package init so every
// OpenTask invocation pays only the MatchString cost, not the compile
// cost. The pattern is identical to the shape described in ADR 0005
// and invariant 15.
var taskIDPattern = regexp.MustCompile(`^agent-infra-[a-z0-9]{8}$`)

// OpenTask creates a new task record with status=open and returns its
// generated TaskID. Files must be non-empty, bounded by
// cfg.MaxTaskFiles, every path absolute, and are sorted+deduplicated
// here before Create (callers need not pre-sort).
//
// Title is the human-readable summary; it must be non-empty. No upper
// bound is enforced beyond internal/tasks's MaxValueSize check on the
// encoded record.
//
// Invariants asserted (panics on violation — programmer errors):
//
//	1 (ctx non-nil), 4 (files shape), 11 (status=open iff claimed_by
//	empty), 15 (generated TaskID matches ADR 0005 shape).
//
// Other underlying errors are returned wrapped; a CAS-Create collision
// on a generated ID is a broken-generator condition that panics via
// assert.Postcondition rather than silently retrying.
func (c *Coord) OpenTask(
	ctx context.Context, title string, files []string,
) (TaskID, error) {
	c.assertOpen("OpenTask")
	c.assertOpenTaskPreconditions(ctx, title, files)
	prepped := sortDedupFiles(files)
	assert.Postcondition(
		sort.StringsAreSorted(prepped),
		"coord.OpenTask: post-sort invariant violated",
	)
	id := generateTaskID()
	assert.Postcondition(
		taskIDPattern.MatchString(string(id)),
		"coord.OpenTask: generated TaskID %q violates ADR 0005 shape",
		id,
	)
	now := time.Now().UTC()
	rec := tasks.Task{
		ID:            string(id),
		Title:         title,
		Status:        tasks.StatusOpen,
		Files:         prepped,
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: tasks.SchemaVersion,
	}
	if err := c.sub.tasks.Create(ctx, rec); err != nil {
		return "", translateCreateErr(id, err)
	}
	return id, nil
}

// assertOpenTaskPreconditions panics on any invariant-1 or -4 violation
// plus the non-empty-title precondition. Kept separate so OpenTask
// itself fits the 70-line funlen cap. Every check here maps directly to
// a clause in docs/invariants.md or the OpenTask spec.
func (c *Coord) assertOpenTaskPreconditions(
	ctx context.Context, title string, files []string,
) {
	assert.NotNil(ctx, "coord.OpenTask: ctx is nil")
	assert.NotEmpty(title, "coord.OpenTask: title is empty")
	assert.Precondition(
		len(files) > 0, "coord.OpenTask: files is empty",
	)
	assert.Precondition(
		len(files) <= c.cfg.Tuning.MaxTaskFiles,
		"coord.OpenTask: len(files)=%d exceeds MaxTaskFiles=%d",
		len(files), c.cfg.Tuning.MaxTaskFiles,
	)
	for _, f := range files {
		assert.Precondition(
			filepath.IsAbs(f),
			"coord.OpenTask: file not absolute: %q", f,
		)
	}
}

// sortDedupFiles returns a copy of in, sorted ascending and with
// consecutive duplicates collapsed. The input is never mutated so
// callers keep ownership of the slice they passed. A fresh backing
// array also avoids the surprise of the returned slice sharing memory
// with the caller's view after the Create.
func sortDedupFiles(in []string) []string {
	sorted := make([]string, len(in))
	copy(sorted, in)
	sort.Strings(sorted)
	out := sorted[:0]
	for i, f := range sorted {
		if i == 0 || f != sorted[i-1] {
			out = append(out, f)
		}
	}
	return out
}

// generateTaskID returns a freshly generated TaskID with the shape
// fixed by ADR 0005: "agent-infra-" followed by taskIDSuffixLen
// lowercase alphanumeric characters drawn uniformly from
// taskIDAlphabet. Entropy comes from crypto/rand so the output is
// unguessable; math/rand is never used because a predictable generator
// would let a misbehaving agent collide IDs on purpose.
//
// A crypto/rand read failure here is unrecoverable and an internal
// contract violation (the OS entropy source must be available), so we
// surface it via assert.NoError rather than threading an error return
// through the OpenTask signature. The shape is further checked by the
// caller against taskIDPattern to catch any construction regression.
func generateTaskID() TaskID {
	buf := make([]byte, taskIDSuffixLen)
	_, err := rand.Read(buf)
	assert.NoError(err, "coord.OpenTask: crypto/rand.Read failed")
	alpha := taskIDAlphabet
	n := byte(len(alpha))
	for i, b := range buf {
		buf[i] = alpha[b%n]
	}
	return TaskID(taskIDProject + "-" + string(buf))
}

// translateCreateErr maps an internal/tasks.Create error into the
// coord-level return value. An ErrAlreadyExists here is an ID
// collision, which under the ADR 0005 generator is a programmer error
// in the generator — retrying would mask the bug, so we panic via
// assert.Postcondition. Every other error is wrapped with the method
// prefix so callers can still errors.Is against tasks-level sentinels
// (including ErrValueTooLarge) through the chain.
func translateCreateErr(id TaskID, err error) error {
	if errors.Is(err, tasks.ErrAlreadyExists) {
		assert.Postcondition(
			false,
			"coord.OpenTask: generated TaskID collided: %q", id,
		)
	}
	return fmt.Errorf("coord.OpenTask: %w", err)
}
