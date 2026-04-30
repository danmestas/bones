package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePlan_ValidPlan(t *testing.T) {
	_, violations, err := validatePlan(filepath.Join("testdata", "valid_plan.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected no violations, got: %v", violations)
	}
}

func TestValidatePlan_MissingSlotAnnotation(t *testing.T) {
	_, violations, err := validatePlan(filepath.Join("testdata", "invalid_missing_slot.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("expected violation for missing slot")
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "missing [slot:") {
		t.Fatalf("expected 'missing [slot:' in violations, got: %s", joined)
	}
}

func TestValidatePlan_DirectoryOverlap(t *testing.T) {
	_, violations, err := validatePlan(filepath.Join("testdata", "invalid_overlap.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "overlap") {
		t.Fatalf("expected 'overlap' in violations, got: %s", joined)
	}
}

func TestValidatePlan_FilesOutsideSlot(t *testing.T) {
	_, violations, err := validatePlan(filepath.Join("testdata", "invalid_files_outside_slot.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "outside slot directory") {
		t.Fatalf("expected 'outside slot directory' in violations, got: %s", joined)
	}
}

func TestValidatePlan_ListSlotsRoundTrip(t *testing.T) {
	tasks, violations, err := validatePlan(filepath.Join("testdata", "valid_plan.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("unexpected violations: %v", violations)
	}

	list := buildSlotList(tasks)

	if len(list.Slots) != 2 {
		t.Fatalf("expected 2 slots, got %d: %v", len(list.Slots), list.Slots)
	}
	if list.Slots[0].Name != "alpha" {
		t.Errorf("slot[0] name: got %q, want %q", list.Slots[0].Name, "alpha")
	}
	if list.Slots[1].Name != "beta" {
		t.Errorf("slot[1] name: got %q, want %q", list.Slots[1].Name, "beta")
	}
	if len(list.Slots[0].Tasks) != 1 {
		t.Errorf("slot alpha tasks: got %d, want 1", len(list.Slots[0].Tasks))
	}
	if len(list.Slots[1].Tasks) != 1 {
		t.Errorf("slot beta tasks: got %d, want 1", len(list.Slots[1].Tasks))
	}

	b, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var got slotList
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(got.Slots) != 2 {
		t.Fatalf("after round-trip: expected 2 slots, got %d", len(got.Slots))
	}
}

func TestValidatePlan_InvalidPlanSuppressedJSON(t *testing.T) {
	_, violations, err := validatePlan(filepath.Join("testdata", "invalid_missing_slot.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("expected violations for invalid plan")
	}
}

// TestRunValidatePlan_CleanReturnsZero verifies the
// runValidatePlan contract: clean plans return exit=0 with empty
// errors and a populated slot list. The result shape is the JSON
// orchestrator scripts pipe through jq/json.load.
func TestRunValidatePlan_CleanReturnsZero(t *testing.T) {
	res, exit := runValidatePlan(filepath.Join("testdata", "valid_plan.md"))
	if exit != 0 {
		t.Fatalf("exit: got %d, want 0; errors=%v", exit, res.Errors)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
	if len(res.Slots) != 2 {
		t.Fatalf("slots: got %d, want 2", len(res.Slots))
	}
	// Always-present fields: even with zero errors, Errors must
	// marshal as `[]` not `null` so consumers don't have to
	// special-case the missing-key shape.
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"errors":[]`) {
		t.Errorf("errors field not empty array: %s", b)
	}
}

// TestRunValidatePlan_ViolationsReturnsOne pins the failure-mode
// output: violations land in res.Errors as an array of strings, exit
// is 1, and the slots that DID parse cleanly are still surfaced so
// orchestrators that want to act on partial data can.
func TestRunValidatePlan_ViolationsReturnsOne(t *testing.T) {
	res, exit := runValidatePlan(filepath.Join("testdata", "invalid_files_outside_slot.md"))
	if exit != 1 {
		t.Fatalf("exit: got %d, want 1", exit)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected at least one error in res.Errors")
	}
	joined := strings.Join(res.Errors, "\n")
	if !strings.Contains(joined, "outside slot directory") {
		t.Errorf("expected 'outside slot directory' in errors, got: %s", joined)
	}
}

// TestRunValidatePlan_ParseErrorReturnsTwo pins the parse-error
// shape: missing-file or IO failure produces a JSON-emittable
// result with the error text in res.Errors and exit=2 (distinct
// from exit=1 so callers can tell "your plan is wrong" apart from
// "your path is wrong").
func TestRunValidatePlan_ParseErrorReturnsTwo(t *testing.T) {
	res, exit := runValidatePlan(filepath.Join("testdata", "this_file_does_not_exist.md"))
	if exit != 2 {
		t.Fatalf("exit: got %d, want 2", exit)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected error in res.Errors")
	}
}
