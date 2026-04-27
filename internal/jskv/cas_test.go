package jskv_test

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/bones/internal/jskv"
)

// TestIsConflict_Nil pins the happy-path no-error shortcut — the
// predicate is called on every successful KV write, so mis-classifying
// nil as a conflict would wedge every caller in an infinite retry.
func TestIsConflict_Nil(t *testing.T) {
	if jskv.IsConflict(nil) {
		t.Fatalf("IsConflict(nil) = true, want false")
	}
}

// TestIsConflict_ErrKeyExists covers the Create-collision path —
// the sentinel returned by jetstream.KeyValue.Create when the key
// already has a value. Both holds.Announce and tasks.Create rely on
// this being classified as a retriable CAS conflict.
func TestIsConflict_ErrKeyExists(t *testing.T) {
	if !jskv.IsConflict(jetstream.ErrKeyExists) {
		t.Fatalf("IsConflict(ErrKeyExists) = false, want true")
	}
}

// TestIsConflict_WrappedErrKeyExists proves errors.Is semantics hold
// through a fmt.Errorf %w wrap — callers often wrap before inspecting
// and the predicate must see through the wrap.
func TestIsConflict_WrappedErrKeyExists(t *testing.T) {
	wrapped := errors.Join(errors.New("prelude"), jetstream.ErrKeyExists)
	if !jskv.IsConflict(wrapped) {
		t.Fatalf("IsConflict(wrapped ErrKeyExists) = false, want true")
	}
}

// TestIsConflict_APIErrorCode covers the Update-revision-mismatch
// path. The server returns JSErrCodeStreamWrongLastSequence (10071)
// which the client surfaces as *jetstream.APIError. ErrKeyExists
// shares the same code via the APIError unwrap chain, but Update
// does not go through ErrKeyExists — it surfaces the APIError raw.
func TestIsConflict_APIErrorCode(t *testing.T) {
	apiErr := &jetstream.APIError{
		ErrorCode: jetstream.JSErrCodeStreamWrongLastSequence,
	}
	if !jskv.IsConflict(apiErr) {
		t.Fatalf("IsConflict(APIError{10071}) = false, want true")
	}
}

// TestIsConflict_UnrelatedError pins the negative case. Any error
// that is neither ErrKeyExists nor an APIError carrying code 10071
// must return false; otherwise genuine transport errors would be
// silently retried and mask real failures.
func TestIsConflict_UnrelatedError(t *testing.T) {
	if jskv.IsConflict(errors.New("boom")) {
		t.Fatalf("IsConflict(random) = true, want false")
	}
}

// TestIsConflict_OtherAPIErrorCode pins the one failure mode that the
// code-path equality guards against: an APIError carrying a different
// ErrorCode must NOT be classified as a CAS conflict. Without this,
// unrelated jetstream failures (stream-not-found, bucket-missing)
// would send callers into the retry loop.
func TestIsConflict_OtherAPIErrorCode(t *testing.T) {
	apiErr := &jetstream.APIError{ErrorCode: 10059}
	if jskv.IsConflict(apiErr) {
		t.Fatalf("IsConflict(APIError{10059}) = true, want false")
	}
}

// TestMaxRetries pins the bound exported to holds/tasks/etc. A change
// here ripples to every caller's worst-case latency budget, so the
// value is pinned by test rather than comment.
func TestMaxRetries(t *testing.T) {
	if jskv.MaxRetries != 8 {
		t.Fatalf("MaxRetries = %d, want 8", jskv.MaxRetries)
	}
}
