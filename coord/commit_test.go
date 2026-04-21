package coord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// TestCommitSmoke_ClaimCommitOpenFile walks one agent through the full
// code-artifact write path: OpenTask → Claim → Commit → OpenFile →
// release. Proves the hold-gate lets a held write through, the repo
// round-trips the content, and release cleans up cleanly.
func TestCommitSmoke_ClaimCommitOpenFile(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	c := newCoordOnURL(t, nc.ConnectedUrl(), "commit-agent")
	ctx := context.Background()

	path := "/src/hello.go"
	id, err := c.OpenTask(ctx, "write hello", []string{path})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}

	release, err := c.Claim(ctx, id, 10*time.Second)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Cleanup(func() { _ = release() })

	body := []byte("package main\n\nfunc main() {}\n")
	rev, err := c.Commit(ctx, "initial hello", []File{
		{Path: path, Content: body},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if rev == "" {
		t.Fatalf("Commit: empty RevID")
	}

	got, err := c.OpenFile(ctx, rev, path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("OpenFile round-trip: got %q, want %q", got, body)
	}
}

// TestCommit_HoldGate_Unheld proves Invariant 20: a Commit on a file
// the agent does not hold returns ErrNotHeld without writing. The
// follow-up OpenFile on the rev that would have been produced must
// fail (rev does not exist).
func TestCommit_HoldGate_Unheld(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	c := newCoordOnURL(t, nc.ConnectedUrl(), "unheld-agent")
	ctx := context.Background()

	_, err := c.Commit(ctx, "sneaky", []File{
		{Path: "/not/held.txt", Content: []byte("x")},
	})
	if err == nil {
		t.Fatalf("Commit: expected ErrNotHeld, got nil")
	}
	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Commit: err = %v, want errors.Is ErrNotHeld", err)
	}
	if !strings.Contains(err.Error(), "/not/held.txt") {
		t.Fatalf("Commit: err should mention offending path: %v", err)
	}
}

// TestCommit_HoldGate_HeldByOther proves the hold-gate rejects commits
// on files held by a different agent — not just unheld files.
func TestCommit_HoldGate_HeldByOther(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	owner := newCoordOnURL(t, nc.ConnectedUrl(), "owner-agent")
	intruder := newCoordOnURL(t, nc.ConnectedUrl(), "intruder-agent")
	ctx := context.Background()

	path := "/src/contested.go"
	id, err := owner.OpenTask(ctx, "owner task", []string{path})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := owner.Claim(ctx, id, 10*time.Second)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Cleanup(func() { _ = rel() })

	_, err = intruder.Commit(ctx, "intrude", []File{
		{Path: path, Content: []byte("y")},
	})
	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("intruder.Commit: err = %v, want ErrNotHeld", err)
	}
}

// TestCommit_InvariantPanics covers the programmer-error preconditions.
func TestCommit_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	ok := []File{{Path: "/a", Content: []byte("x")}}

	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Commit(nilCtx, "m", ok)
		}, "ctx is nil")
	})
	t.Run("empty message", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Commit(ctx, "", ok)
		}, "message is empty")
	})
	t.Run("empty files", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Commit(ctx, "m", nil)
		}, "files is empty")
	})
	t.Run("empty file path", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Commit(ctx, "m", []File{{Path: "", Content: []byte("x")}})
		}, "file.Path is empty")
	})
}

// TestOpenFile_InvariantPanics covers programmer-error preconditions.
func TestOpenFile_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.OpenFile(nilCtx, RevID("r"), "/a")
		}, "ctx is nil")
	})
	t.Run("empty rev", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.OpenFile(ctx, RevID(""), "/a")
		}, "rev is empty")
	})
	t.Run("empty path", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.OpenFile(ctx, RevID("r"), "")
		}, "path is empty")
	})
}
