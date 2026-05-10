package cli

import (
	"reflect"
	"testing"
)

// TestApplyContext_DoesNotMutateInput pins the load-bearing contract
// for #347. applyContext is called from buildUpdateMutator with
// `t.Context` whose underlying map is shared with the pre-image
// `before.Context`. If applyContext mutated in place, before.Context
// would change too, fooling diffTaskFields into seeing no change and
// silently dropping the write.
//
// Concretely: this test fails on the pre-#347 implementation and
// passes on the new copy-on-write one.
func TestApplyContext_DoesNotMutateInput(t *testing.T) {
	original := map[string]string{"foo": "bar"}
	originalCopy := map[string]string{"foo": "bar"}

	got := applyContext(original, []string{"foo=baz"})

	if !reflect.DeepEqual(original, originalCopy) {
		t.Errorf("applyContext mutated input map: got %v, want %v",
			original, originalCopy)
	}
	if got["foo"] != "baz" {
		t.Errorf("applyContext returned %v, want context with foo=baz", got)
	}
	// Same-pointer check belt-and-braces. A shared backing map would
	// allow callers to observe mutations even when DeepEqual happens
	// to coincide.
	if reflect.ValueOf(original).Pointer() == reflect.ValueOf(got).Pointer() {
		t.Errorf("applyContext returned the input map; must return a new map")
	}
}

// TestApplyContext_PreservesExistingKeys verifies that a NEW pair
// merges with existing entries rather than replacing the whole map.
// Distinct from the mutation test: this asserts merge semantics on
// the returned value.
func TestApplyContext_PreservesExistingKeys(t *testing.T) {
	in := map[string]string{"a": "1", "b": "2"}
	got := applyContext(in, []string{"c=3"})

	want := map[string]string{"a": "1", "b": "2", "c": "3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestApplyContext_NewKeyOverwritesExisting confirms last-pair-wins
// for repeated keys. Pre-existing behavior; pinned here so the
// copy-on-write rewrite doesn't accidentally regress it.
func TestApplyContext_NewKeyOverwritesExisting(t *testing.T) {
	in := map[string]string{"k": "old"}
	got := applyContext(in, []string{"k=new"})

	if got["k"] != "new" {
		t.Errorf("got k=%q, want k=new", got["k"])
	}
	if in["k"] != "old" {
		t.Errorf("input k mutated to %q, want still old", in["k"])
	}
}

// TestApplyContext_NilExistingProducesFreshMap pins the nil-input
// branch — the case where the FIRST context update on a task with
// no existing context goes through. This branch worked in the buggy
// implementation; the test ensures the rewrite preserves it.
func TestApplyContext_NilExistingProducesFreshMap(t *testing.T) {
	got := applyContext(nil, []string{"k=v"})
	want := map[string]string{"k": "v"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestApplyContext_EmptyPairsReturnsInput confirms the early-return
// branch is unchanged: zero pairs means no-op, return whatever was
// passed in (including nil).
func TestApplyContext_EmptyPairsReturnsInput(t *testing.T) {
	in := map[string]string{"a": "1"}
	got := applyContext(in, nil)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(in).Pointer() {
		t.Errorf("with empty pairs, applyContext should return the input map verbatim")
	}

	if applyContext(nil, nil) != nil {
		t.Errorf("applyContext(nil, nil) should return nil")
	}
}
