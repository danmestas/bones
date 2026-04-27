package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_ValidPlan(t *testing.T) {
	_, violations, err := validate(filepath.Join("testdata", "valid_plan.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected no violations, got: %v", violations)
	}
}

func TestValidate_MissingSlotAnnotation(t *testing.T) {
	_, violations, err := validate(filepath.Join("testdata", "invalid_missing_slot.md"))
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

func TestValidate_DirectoryOverlap(t *testing.T) {
	_, violations, err := validate(filepath.Join("testdata", "invalid_overlap.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "overlap") {
		t.Fatalf("expected 'overlap' in violations, got: %s", joined)
	}
}

func TestValidate_FilesOutsideSlot(t *testing.T) {
	_, violations, err := validate(filepath.Join("testdata", "invalid_files_outside_slot.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "outside slot directory") {
		t.Fatalf("expected 'outside slot directory' in violations, got: %s", joined)
	}
}

func TestListSlots_RoundTrip(t *testing.T) {
	tasks, violations, err := validate(filepath.Join("testdata", "valid_plan.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("unexpected violations: %v", violations)
	}

	list := buildSlotList(tasks)

	// valid_plan.md has two slots: alpha and beta.
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

	// Round-trip through JSON: must marshal and unmarshal cleanly.
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

func TestListSlots_InvalidPlanSuppressedJSON(t *testing.T) {
	// Validation failure must be detected; buildSlotList should not be called
	// by main in that case. Test that validate returns violations for an
	// invalid plan even when tasks are non-nil.
	_, violations, err := validate(filepath.Join("testdata", "invalid_missing_slot.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("expected violations for invalid plan")
	}
}
