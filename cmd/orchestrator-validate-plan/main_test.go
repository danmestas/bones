package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_ValidPlan(t *testing.T) {
	violations, err := validate(filepath.Join("testdata", "valid_plan.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected no violations, got: %v", violations)
	}
}

func TestValidate_MissingSlotAnnotation(t *testing.T) {
	violations, err := validate(filepath.Join("testdata", "invalid_missing_slot.md"))
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
	violations, err := validate(filepath.Join("testdata", "invalid_overlap.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "overlap") {
		t.Fatalf("expected 'overlap' in violations, got: %s", joined)
	}
}

func TestValidate_FilesOutsideSlot(t *testing.T) {
	violations, err := validate(filepath.Join("testdata", "invalid_files_outside_slot.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "outside slot directory") {
		t.Fatalf("expected 'outside slot directory' in violations, got: %s", joined)
	}
}
